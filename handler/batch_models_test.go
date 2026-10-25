package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/atopos31/llmio/models"
	"gorm.io/gorm"
)

// 「从供应商导入」两步后端的契约（方案 §4.2.1）。建议来源走 batch 测试
// 同一个 stub（installBatchStubMeta 装的那份索引：batch-a 字段齐全、
// batch-partial 只有 tool_call）。

func callBatchModels(t *testing.T, body any) (int, BatchModelsResponse) {
	t.Helper()
	w := doJSON(t, BatchCreateModels, http.MethodPost, "/api/models/batch", body, nil)
	var env struct {
		Code int                 `json:"code"`
		Data BatchModelsResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("解响应: %v（原文 %s）", err, w.Body.String())
	}
	return env.Code, env.Data
}

func TestBatchCreateModelsAppliesServerDefaults(t *testing.T) {
	setupBatchDB(t)

	code, resp := callBatchModels(t, map[string]any{
		"names":    []string{"imp-a", "imp-b"},
		"defaults": nil,
	})
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Created) != 2 {
		t.Fatalf("created = %d，期望 2", len(resp.Created))
	}
	for _, row := range resp.Created {
		if row.TimeOut != 300 || row.MaxRetry != 3 || row.Strategy != "lottery" {
			t.Errorf("%s defaults = (%d, %d, %q)，期望服务端默认 (300, 3, lottery)",
				row.Name, row.TimeOut, row.MaxRetry, row.Strategy)
		}
		if row.DisplayOrder == 0 {
			t.Errorf("%s display_order 应在事务内分配", row.Name)
		}
	}
}

func TestBatchCreateModelsExplicitValuesAndValidation(t *testing.T) {
	setupBatchDB(t)

	breaker := true
	code, resp := callBatchModels(t, map[string]any{
		"names": []string{"imp-c"},
		"defaults": map[string]any{
			"max_retry": 5, "time_out": 120, "strategy": "rotor", "breaker": breaker,
		},
	})
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	row := resp.Created[0]
	if row.MaxRetry != 5 || row.TimeOut != 120 || row.Strategy != "rotor" || row.Breaker == nil || !*row.Breaker {
		t.Errorf("显式值应原样落库：%+v", row)
	}

	// 同名幂等：重交全 skipped
	code, resp = callBatchModels(t, map[string]any{"names": []string{"imp-c"}})
	if code != http.StatusOK || len(resp.Created) != 0 || len(resp.Skipped) != 1 || resp.Skipped[0].Reason != "exists" {
		t.Fatalf("重交应全 skipped：code=%d resp=%+v", code, resp)
	}

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"max_retry=0 是毒药", map[string]any{"names": []string{"x1"}, "defaults": map[string]any{"max_retry": 0}}},
		{"time_out=0 是毒药", map[string]any{"names": []string{"x1"}, "defaults": map[string]any{"time_out": 0}}},
		{"strategy 白名单", map[string]any{"names": []string{"x1"}, "defaults": map[string]any{"strategy": "random"}}},
		{"名字重复", map[string]any{"names": []string{"dup", "dup"}}},
		{"名字空白", map[string]any{"names": []string{"  "}}},
		{"名字为空", map[string]any{"names": []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := callBatchModels(t, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("业务码 = %d，期望 400", code)
			}
		})
	}
}

func TestBatchBackfillFillsOnlyVacantSlots(t *testing.T) {
	setupBatchDB(t)

	// 三行：全空位（老行 nil 能力 + 0 价）/ 显式值（非 nil 能力 + 非 0 价）/
	// 源里没有的模型
	rows := []models.ModelWithProvider{
		{
			ModelID: 1, ProviderID: 1, ProviderModel: "batch-a",
			// 老行形态：能力列 NULL、价格 0、币种空
			InputPrice: floatPtr(0), CacheReadPrice: floatPtr(0), OutputPrice: floatPtr(0),
			Status: boolPtr(true), Currency: "CNY",
		},
		{
			// 与第一行同名（batch-a）但挂在模型 2 上——同一个建议要复用到第二行
			ModelID:       2,
			ProviderID:    1,
			ProviderModel: "batch-a",
			ToolCall:      boolPtr(false), StructuredOutput: boolPtr(true), Image: boolPtr(false),
			InputPrice: floatPtr(0.5), CacheReadPrice: floatPtr(0.1), OutputPrice: floatPtr(2),
			Status: boolPtr(true), Currency: "CNY",
		},
		{
			ModelID: 2, ProviderID: 1, ProviderModel: "no-such-model",
			ToolCall:   boolPtr(false),
			InputPrice: floatPtr(0), CacheReadPrice: floatPtr(0), OutputPrice: floatPtr(0),
			Status: boolPtr(true), Currency: "CNY",
		},
	}
	for i := range rows {
		if err := models.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	w := doJSON(t, BatchBackfillMetadata, http.MethodPost, "/api/model-providers/batch/backfill",
		map[string]any{"ids": []uint{rows[0].ID, rows[1].ID, rows[2].ID, 424242}}, nil)
	var env struct {
		Code int              `json:"code"`
		Data BackfillResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("解响应: %v（原文 %s）", err, w.Body.String())
	}
	if env.Code != http.StatusOK {
		t.Fatalf("业务码 = %d（原文 %s）", env.Code, w.Body.String())
	}

	// 空位行：能力补齐 + 三档价格补齐 + 币种联动 USD
	filled0 := env.Data.Filled[0]
	if filled0.ID != rows[0].ID {
		t.Fatalf("filled[0].ID = %d，期望空位行", filled0.ID)
	}
	if _, ok := filled0.Fields["tool_call"]; !ok {
		t.Errorf("nil 能力位应被补：%+v", filled0.Fields)
	}
	if v, ok := filled0.Fields["input_price"].(float64); !ok || v != 3 {
		t.Errorf("input_price 应补成建议价 3：%+v", filled0.Fields)
	}
	if v, ok := filled0.Fields["currency"].(string); !ok || v != "USD" {
		t.Errorf("补价后币种应联动 USD：%+v", filled0.Fields)
	}
	got0, err := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", rows[0].ID).First(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got0.ToolCall == nil || !*got0.ToolCall {
		t.Errorf("tool_call = %v，期望被补成 true", got0.ToolCall)
	}
	if got0.InputPrice == nil || *got0.InputPrice != 3 {
		t.Errorf("input_price = %v，期望 3", got0.InputPrice)
	}
	if got0.Currency != "USD" {
		t.Errorf("currency = %q，期望 USD", got0.Currency)
	}

	// 显式值行：一个字段都不动，进 unchanged
	if len(env.Data.Unchanged) != 1 || env.Data.Unchanged[0] != rows[1].ID {
		t.Fatalf("unchanged = %v，期望显式值那行", env.Data.Unchanged)
	}
	got1, _ := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", rows[1].ID).First(context.Background())
	if got1.InputPrice == nil || *got1.InputPrice != 0.5 {
		t.Errorf("显式价格不该被动：%v", got1.InputPrice)
	}

	// 未命中行与不存在的 id 各归各位
	if len(env.Data.Unfilled) != 1 || env.Data.Unfilled[0].ProviderModel != "no-such-model" {
		t.Fatalf("unfilled = %+v", env.Data.Unfilled)
	}
	if len(env.Data.NotFound) != 1 || env.Data.NotFound[0] != 424242 {
		t.Fatalf("not_found = %v", env.Data.NotFound)
	}
}

func TestBatchBackfillRequiresPolicyEnabled(t *testing.T) {
	setupBatchDB(t)
	if _, err := gorm.G[models.Config](models.DB).
		Where("key = ?", models.KeyModelAutofillPolicy).
		Update(context.Background(), "value", `{"enabled":false}`); err != nil {
		t.Fatalf("关策略: %v", err)
	}

	w := doJSON(t, BatchBackfillMetadata, http.MethodPost, "/api/model-providers/batch/backfill",
		map[string]any{"ids": []uint{1}}, nil)
	if code := envelopeCode(t, w); code != http.StatusBadRequest {
		t.Fatalf("业务码 = %d，期望 400（策略关着补填该拒绝）", code)
	}
}
