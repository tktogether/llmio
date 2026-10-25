package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/service/modelmeta"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 批量关联端点的行为契约（docs/plans/association-batch-operations.md §4.1.1）。
// 建议来源全部走 stub（与 modelmeta_test.go 同一个接缝），整组不联网。

// batchResp 是批量创建响应里测试关心的部分。
type batchResp struct {
	DryRun   bool                `json:"dry_run"`
	Created  []batchCreatedRow   `json:"created"`
	Skipped  []BatchSkippedItem  `json:"skipped"`
	Failed   []BatchFailedItem   `json:"failed"`
	Unfilled []BatchUnfilledItem `json:"unfilled"`
}

type batchCreatedRow struct {
	ID            uint                 `json:"ID"`
	ModelID       uint                 `json:"ModelID"`
	ProviderModel string               `json:"ProviderModel"`
	ToolCall      *bool                `json:"ToolCall"`
	Image         *bool                `json:"Image"`
	InputPrice    *float64             `json:"InputPrice"`
	Currency      string               `json:"Currency"`
	Weight        int                  `json:"Weight"`
	Autofill      *BatchAutofillReport `json:"Autofill"`
}

// setupBatchDB 建库、建索引、写两条模型一条上游，并换上不联网的建议管理器。
//
// 索引必须建：批量创建"撞唯一索引转 skipped"与"软删行可重建"两条语义都
// 依赖 partial unique index，测试库要与生产同一套建法（SQL 常量在 models 包）。
func setupBatchDB(t *testing.T) {
	t.Helper()
	path := t.TempDir() + "\\batch.db"
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&models.Model{}, &models.Provider{}, &models.ModelWithProvider{}, &models.Config{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	if err := db.Exec(models.AssociationUniqueIndexSQL).Error; err != nil {
		t.Fatalf("create unique index: %v", err)
	}

	m1 := models.Model{Name: "batch-a", MaxRetry: 3, TimeOut: 300, Strategy: "lottery"}
	m1.ID = 1
	m2 := models.Model{Name: "batch-b", MaxRetry: 3, TimeOut: 300, Strategy: "lottery"}
	m2.ID = 2
	provider := models.Provider{Name: "批测上游", Type: "anthropic", Config: `{"base_url":"https://api.anthropic.com"}`}
	provider.ID = 1
	if err := db.Create(&[]models.Model{m1, m2}).Error; err != nil {
		t.Fatalf("seed models: %v", err)
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	// 策略值留空：LoadPolicy 退回默认（Enabled=true）。
	if err := db.Create(&models.Config{Key: models.KeyModelAutofillPolicy, Value: ""}).Error; err != nil {
		t.Fatalf("seed config: %v", err)
	}

	prevDB := models.DB
	models.DB = db
	t.Cleanup(func() {
		models.DB = prevDB
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	installBatchStubMeta(t)
}

// installBatchStubMeta 装一份索引：batch-a 字段齐全（含三档 USD 价格），
// batch-partial 只有 tool_call，别的名字都不在源里。
func installBatchStubMeta(t *testing.T) {
	t.Helper()
	entries := []modelmeta.Entry{
		{
			Source: modelmeta.SourceModelsDev, Provider: "anthropic", ProviderName: "Anthropic",
			Model: "batch-a", Currency: modelmeta.CurrencyUSD,
			ToolCall: boolPtr(true), StructuredOutput: boolPtr(true), Image: boolPtr(true),
			InputPrice: floatPtr(3), CacheReadPrice: floatPtr(0.3), OutputPrice: floatPtr(15),
		},
		{
			Source: modelmeta.SourceModelsDev, Provider: "anthropic", ProviderName: "Anthropic",
			Model: "batch-partial", Currency: modelmeta.CurrencyUSD,
			ToolCall: boolPtr(true),
		},
	}
	cat := modelmeta.NewCatalog(modelmeta.SourceModelsDev, time.Now(), entries,
		map[string]modelmeta.ProviderInfo{"anthropic": {Npm: "@ai-sdk/anthropic"}})
	mgr := modelmeta.NewManager(stubMetaSource{cat: cat})
	mgr.SetCatalog(cat)

	prev := modelMetaManager
	modelMetaManager = func() *modelmeta.Manager { return mgr }
	t.Cleanup(func() { modelMetaManager = prev })
}

func callBatchCreate(t *testing.T, body any) (int, batchResp) {
	t.Helper()
	w := doJSON(t, BatchCreateModelProviders, http.MethodPost, "/api/model-providers/batch", body, nil)

	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("解响应: %v（原文 %s）", err, w.Body.String())
	}
	var resp batchResp
	if env.Code == http.StatusOK {
		if err := json.Unmarshal(env.Data, &resp); err != nil {
			t.Fatalf("解 data: %v（原文 %s）", err, env.Data)
		}
	}
	return env.Code, resp
}

func batchBody(modelIDs []uint, providerModels []string, defaults map[string]any) map[string]any {
	if defaults == nil {
		defaults = map[string]any{"weight": 1}
	}
	return map[string]any{
		"model_ids":       modelIDs,
		"provider_id":     1,
		"provider_models": providerModels,
		"defaults":        defaults,
	}
}

func loadRow(t *testing.T, modelID uint, pm string) models.ModelWithProvider {
	t.Helper()
	row, err := gorm.G[models.ModelWithProvider](models.DB).
		Where("model_id = ? AND provider_id = 1 AND provider_model = ?", modelID, pm).
		First(context.Background())
	if err != nil {
		t.Fatalf("load row (%d, %s): %v", modelID, pm, err)
	}
	return row
}

func assocCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := models.DB.Model(&models.ModelWithProvider{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestBatchDefaultsMirrorSingleRequest：批量 defaults 与单条请求体的字段
// 集合一一对应（除移到信封上的 model_id / provider_id / provider_name）。
// 这条测试在，新增字段漏掉任何一边都会直接红——字段漂移是批量端点最
// 容易长出来的债。
func TestBatchDefaultsMirrorSingleRequest(t *testing.T) {
	skip := map[string]bool{"ModelID": true, "ProviderID": true, "ProviderModel": true}

	single := reflect.TypeOf(ModelWithProviderRequest{})
	batch := reflect.TypeOf(BatchAssociationDefaults{})

	singleFields := map[string]string{}
	for i := range single.NumField() {
		f := single.Field(i)
		singleFields[f.Name] = f.Tag.Get("json")
	}
	for i := range batch.NumField() {
		f := batch.Field(i)
		want, ok := singleFields[f.Name]
		if !ok {
			t.Errorf("BatchAssociationDefaults.%s 在 ModelWithProviderRequest 里没有对应字段", f.Name)
			continue
		}
		if want != f.Tag.Get("json") {
			t.Errorf("%s 的 json tag 不一致：单条 %q vs 批量 %q", f.Name, want, f.Tag.Get("json"))
		}
	}
	for name := range singleFields {
		if skip[name] {
			continue
		}
		if _, ok := batch.FieldByName(name); !ok {
			t.Errorf("ModelWithProviderRequest.%s 在 BatchAssociationDefaults 里没有对应字段", name)
		}
	}
}

// TestBatchCreateFillsNullSlotsAndReports：nil 空位被建议补上，且补了什么
// 在响应里看得见；价格补了，币种跟着建议走（USD），不跟 defaults 里的 CNY。
func TestBatchCreateFillsNullSlotsAndReports(t *testing.T) {
	setupBatchDB(t)

	code, resp := callBatchCreate(t, batchBody([]uint{1}, []string{"batch-a"}, map[string]any{
		"weight":   2,
		"currency": "CNY",
	}))
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d，期望 200", code)
	}
	if len(resp.Created) != 1 || len(resp.Skipped) != 0 || len(resp.Failed) != 0 {
		t.Fatalf("created=%d skipped=%d failed=%d，期望 1/0/0", len(resp.Created), len(resp.Skipped), len(resp.Failed))
	}

	row := loadRow(t, 1, "batch-a")
	if row.ToolCall == nil || !*row.ToolCall || row.Image == nil || !*row.Image {
		t.Errorf("能力位应被建议补成 true：tool=%v image=%v", row.ToolCall, row.Image)
	}
	if row.InputPrice == nil || *row.InputPrice != 3 {
		t.Errorf("input_price = %v，期望按建议补成 3", row.InputPrice)
	}
	if row.Weight != 2 {
		t.Errorf("weight = %d，期望 2（非元数据字段跟 defaults）", row.Weight)
	}
	if row.Currency != "USD" {
		t.Errorf("currency = %q，价格来自建议时币种必须联动成 USD", row.Currency)
	}

	item := resp.Created[0]
	if item.Autofill == nil || len(item.Autofill.Filled) != 6 {
		t.Fatalf("autofill 报告 = %+v，期望补满 6 格", item.Autofill)
	}
	if item.Autofill.Source != modelmeta.SourceModelsDev {
		t.Errorf("autofill source = %q", item.Autofill.Source)
	}
	if len(resp.Unfilled) != 0 {
		t.Errorf("unfilled = %+v，期望空", resp.Unfilled)
	}
}

// TestBatchCreateExplicitValuesBeatAutofill：显式值（含显式 false / 0）是
// 用户意志，建议一个字段都覆盖不了——批量场景"我就要 0 价"的唯一表达。
func TestBatchCreateExplicitValuesBeatAutofill(t *testing.T) {
	setupBatchDB(t)

	code, resp := callBatchCreate(t, batchBody([]uint{1}, []string{"batch-a"}, map[string]any{
		"weight":      1,
		"tool_call":   false,
		"input_price": 0,
	}))
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}

	row := loadRow(t, 1, "batch-a")
	if row.ToolCall == nil || *row.ToolCall {
		t.Errorf("tool_call = %v，显式 false 不能被建议翻成 true", row.ToolCall)
	}
	if row.InputPrice == nil || *row.InputPrice != 0 {
		t.Errorf("input_price = %v，显式 0 不能被建议覆盖", row.InputPrice)
	}
	if row.Image == nil || !*row.Image {
		t.Errorf("image = %v，nil 空位仍应被建议补 true", row.Image)
	}
	// 显式给过价格 → 币种跟 defaults；defaults 没给 → 兜底 CNY
	if row.Currency != "CNY" {
		t.Errorf("currency = %q，显式价格时跟 defaults（空则 CNY）", row.Currency)
	}
	if resp.Created[0].Autofill != nil && len(resp.Created[0].Autofill.Filled) != 4 {
		t.Errorf("autofill 报告 = %+v，只该记 structured/image/两档价 4 格", resp.Created[0].Autofill)
	}
}

// TestBatchCreateUnmatchedMaterializesDefaults：源里没有的模型照样创建，
// 空位物化成 false / 0，unfilled 用源给出的原因逐条报告。
func TestBatchCreateUnmatchedMaterializesDefaults(t *testing.T) {
	setupBatchDB(t)

	code, resp := callBatchCreate(t, batchBody([]uint{1, 2}, []string{"no-such-model"}, nil))
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Created) != 2 {
		t.Fatalf("created = %d，期望 2", len(resp.Created))
	}

	row := loadRow(t, 1, "no-such-model")
	if row.ToolCall == nil || *row.ToolCall {
		t.Errorf("tool_call = %v，未命中应物化 false", row.ToolCall)
	}
	if row.InputPrice == nil || *row.InputPrice != 0 {
		t.Errorf("input_price = %v，未命中应物化 0", row.InputPrice)
	}
	if row.Currency != "CNY" {
		t.Errorf("currency = %q，无价格可补时兜底 CNY", row.Currency)
	}
	if len(resp.Unfilled) != 1 || resp.Unfilled[0].Reason != modelmeta.ReasonNoModelMatch {
		t.Errorf("unfilled = %+v，期望 no_model_match 一条", resp.Unfilled)
	}
}

// TestBatchCreatePolicyDisabledSkipsAutofill：策略总开关关掉后批量路径
// 不查建议、不产 unfilled——"关了开关还在慢"比"关了开关没生效"更难排查。
func TestBatchCreatePolicyDisabledSkipsAutofill(t *testing.T) {
	setupBatchDB(t)
	if _, err := gorm.G[models.Config](models.DB).
		Where("key = ?", models.KeyModelAutofillPolicy).
		Update(context.Background(), "value", `{"enabled":false}`); err != nil {
		t.Fatalf("关策略: %v", err)
	}

	code, resp := callBatchCreate(t, batchBody([]uint{1}, []string{"batch-a"}, nil))
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}

	row := loadRow(t, 1, "batch-a")
	if row.ToolCall == nil || *row.ToolCall {
		t.Errorf("tool_call = %v，策略关了就该物化 false", row.ToolCall)
	}
	if len(resp.Unfilled) != 0 {
		t.Errorf("unfilled = %+v，策略关了不该报", resp.Unfilled)
	}
	if resp.Created[0].Autofill != nil {
		t.Errorf("autofill 报告 = %+v，策略关了不该有", resp.Created[0].Autofill)
	}
}

// TestBatchCreateDryRunZeroWrites：dry_run 走同一套展开与预查、零写入，
// created 里是"将要写入的完整行"（ID=0）。
func TestBatchCreateDryRunZeroWrites(t *testing.T) {
	setupBatchDB(t)

	body := batchBody([]uint{1}, []string{"batch-a", "no-such-model"}, nil)
	body["dry_run"] = true
	code, resp := callBatchCreate(t, body)
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}

	if n := assocCount(t); n != 0 {
		t.Fatalf("dry_run 后库里多了 %d 行，期望 0", n)
	}
	if !resp.DryRun || len(resp.Created) != 2 {
		t.Fatalf("dry_run=%v created=%d，期望 true/2", resp.DryRun, len(resp.Created))
	}
	for _, item := range resp.Created {
		if item.ID != 0 {
			t.Errorf("dry_run 的 created.ID = %d，期望 0（还没有落库）", item.ID)
		}
	}
}

// TestBatchCreateSkipExistingReportsStatus：已存在行默认跳过并回显当前
// 启用状态——"已存在且已禁用"必须看得见，否则用户以为重新挂好了。
func TestBatchCreateSkipExistingReportsStatus(t *testing.T) {
	setupBatchDB(t)

	existing := models.ModelWithProvider{
		ModelID: 1, ProviderID: 1, ProviderModel: "batch-a",
		ToolCall: boolPtr(false), StructuredOutput: boolPtr(false), Image: boolPtr(false),
		InputPrice: floatPtr(0), CacheReadPrice: floatPtr(0), OutputPrice: floatPtr(0),
		Status: boolPtr(false),
	}
	if err := models.DB.Create(&existing).Error; err != nil {
		t.Fatalf("seed existing: %v", err)
	}

	code, resp := callBatchCreate(t, batchBody([]uint{1}, []string{"batch-a"}, nil))
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Created) != 0 || len(resp.Skipped) != 1 {
		t.Fatalf("created=%d skipped=%d，期望 0/1", len(resp.Created), len(resp.Skipped))
	}
	skipped := resp.Skipped[0]
	if skipped.Reason != "exists" || skipped.ExistingStatus == nil || *skipped.ExistingStatus {
		t.Errorf("skipped = %+v，期望 exists 且 existing_status=false", skipped)
	}
	if n := assocCount(t); n != 1 {
		t.Fatalf("库里 %d 行，期望仍为 1", n)
	}

	// skip_existing=false：撞上已存在行按失败报告，不静默
	code, resp = callBatchCreate(t, func() map[string]any {
		body := batchBody([]uint{1}, []string{"batch-a"}, nil)
		body["skip_existing"] = false
		return body
	}())
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].Reason != "exists" {
		t.Errorf("failed = %+v，期望 exists 一条", resp.Failed)
	}
}

// TestBatchCreateSoftDeletedRowCanBeRecreated：唯一索引是 partial 的，
// 软删行不占名额——"删了再建"不需要先物理清尸。
func TestBatchCreateSoftDeletedRowCanBeRecreated(t *testing.T) {
	setupBatchDB(t)

	old := models.ModelWithProvider{ModelID: 1, ProviderID: 1, ProviderModel: "batch-a", Status: boolPtr(true)}
	if err := models.DB.Create(&old).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := models.DB.Delete(&old).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	code, resp := callBatchCreate(t, batchBody([]uint{1}, []string{"batch-a"}, nil))
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Created) != 1 || len(resp.Skipped) != 0 {
		t.Fatalf("created=%d skipped=%d，期望 1/0（软删行不挡重建）", len(resp.Created), len(resp.Skipped))
	}
}

// TestBatchCreateValidation：入参卫生——空、0 值、批内重复、越界，全部
// 整批 400，不走到逐条失败。
func TestBatchCreateValidation(t *testing.T) {
	setupBatchDB(t)

	oversized := make([]string, 0, 300)
	for i := range 300 {
		oversized = append(oversized, fmt.Sprintf("m-%d", i))
	}

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"model_ids 空", batchBody([]uint{}, []string{"batch-a"}, nil), "model_ids cannot be empty"},
		{"model_id 为 0", batchBody([]uint{0}, []string{"batch-a"}, nil), "invalid value 0"},
		{"model_ids 重复", batchBody([]uint{1, 1}, []string{"batch-a"}, nil), "duplicated model_id"},
		{"provider_models 空", batchBody([]uint{1}, []string{}, nil), "provider_models cannot be empty"},
		{"provider_model 空白", batchBody([]uint{1}, []string{"  "}, nil), "blank name"},
		{"provider_models 重复", batchBody([]uint{1}, []string{"batch-a", "batch-a"}, nil), "duplicated provider_model"},
		{"provider_id 为 0", func() map[string]any {
			b := batchBody([]uint{1}, []string{"batch-a"}, nil)
			b["provider_id"] = 0
			return b
		}(), "provider_id is required"},
		{"行数超限", batchBody([]uint{1, 2}, oversized, nil), "row limit"},
		{"weight 为 0", batchBody([]uint{1}, []string{"batch-a"}, map[string]any{"weight": 0}), "weight"},
		{"currency 非法", batchBody([]uint{1}, []string{"batch-a"}, map[string]any{"weight": 1, "currency": "EUR"}), "currency"},
		{"provider 不存在", func() map[string]any {
			b := batchBody([]uint{1}, []string{"batch-a"}, nil)
			b["provider_id"] = 999
			return b
		}(), "provider not found"},
		{"model 不存在", batchBody([]uint{999}, []string{"batch-a"}, nil), "non-existing model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := callBatchCreate(t, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("业务码 = %d，期望 400", code)
			}
		})
	}
}

func callBatchMutation(t *testing.T, handlerFn func(*gin.Context), body any) (int, BatchMutationResponse) {
	t.Helper()
	w := doJSON(t, handlerFn, http.MethodPost, "/api/model-providers/batch", body, nil)
	var env struct {
		Code int                   `json:"code"`
		Data BatchMutationResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("解响应: %v（原文 %s）", err, w.Body.String())
	}
	return env.Code, env.Data
}

func TestBatchStatusIdempotent(t *testing.T) {
	setupBatchDB(t)

	rows := []models.ModelWithProvider{
		{ModelID: 1, ProviderID: 1, ProviderModel: "r1", Status: boolPtr(true)},
		{ModelID: 1, ProviderID: 1, ProviderModel: "r2", Status: boolPtr(true)},
	}
	for i := range rows {
		if err := models.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	code, resp := callBatchMutation(t, BatchUpdateModelProviderStatus, map[string]any{
		"ids":    []uint{rows[0].ID, rows[1].ID, 424242},
		"status": false,
	})
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Updated) != 2 || len(resp.NotFound) != 1 || resp.NotFound[0] != 424242 {
		t.Fatalf("resp = %+v，期望 updated=2、not_found=[424242]", resp)
	}
	for _, row := range rows {
		got := loadRow(t, 1, row.ProviderModel)
		if got.Status == nil || *got.Status {
			t.Errorf("%s.status = %v，期望 false", row.ProviderModel, got.Status)
		}
	}

	// 重交同一批：not_found 不变，已停用的行再停一次无害
	code, resp = callBatchMutation(t, BatchUpdateModelProviderStatus, map[string]any{
		"ids":    []uint{rows[0].ID, 424242},
		"status": false,
	})
	if code != http.StatusOK || len(resp.Updated) != 1 || len(resp.NotFound) != 1 {
		t.Fatalf("重交 resp = %+v，期望 updated=1、not_found=1", resp)
	}
}

func TestBatchDeleteIdempotent(t *testing.T) {
	setupBatchDB(t)

	row := models.ModelWithProvider{ModelID: 1, ProviderID: 1, ProviderModel: "r1", Status: boolPtr(true)}
	if err := models.DB.Create(&row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	code, resp := callBatchMutation(t, BatchDeleteModelProviders, map[string]any{"ids": []uint{row.ID, 424242}})
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Deleted) != 1 || len(resp.NotFound) != 1 {
		t.Fatalf("resp = %+v，期望 deleted=1、not_found=1", resp)
	}

	// 软删：Unscoped 还查得到，默认作用域查不到
	var alive, total int64
	models.DB.Model(&models.ModelWithProvider{}).Where("id = ?", row.ID).Count(&alive)
	models.DB.Unscoped().Model(&models.ModelWithProvider{}).Where("id = ?", row.ID).Count(&total)
	if alive != 0 || total != 1 {
		t.Fatalf("alive=%d total=%d，期望软删（0/1）", alive, total)
	}

	// 重交：已删的进 not_found，不报错
	code, resp = callBatchMutation(t, BatchDeleteModelProviders, map[string]any{"ids": []uint{row.ID}})
	if code != http.StatusOK || len(resp.NotFound) != 1 {
		t.Fatalf("重交 resp = %+v，期望 not_found=1", resp)
	}
}

// TestBatchCreatePairsMode：pairs 形态逐对建立、不产生笛卡尔交叉——
// 导入向导第二步靠它把"上游模型 → 同名逻辑模型"精确落库。
func TestBatchCreatePairsMode(t *testing.T) {
	setupBatchDB(t)

	code, resp := callBatchCreate(t, map[string]any{
		"provider_id": 1,
		"pairs": []map[string]any{
			{"model_id": 1, "provider_model": "batch-a"},
			{"model_id": 2, "provider_model": "batch-partial"},
		},
		"defaults": map[string]any{"weight": 1},
	})
	if code != http.StatusOK {
		t.Fatalf("业务码 = %d", code)
	}
	if len(resp.Created) != 2 {
		t.Fatalf("created = %d，期望 2", len(resp.Created))
	}

	// 各自的配对存在，交叉组合不存在
	if row := loadRow(t, 1, "batch-a"); row.ID == 0 {
		t.Fatal("(1, batch-a) 应存在")
	}
	if row := loadRow(t, 2, "batch-partial"); row.ID == 0 {
		t.Fatal("(2, batch-partial) 应存在")
	}
	var cross int64
	models.DB.Model(&models.ModelWithProvider{}).
		Where("model_id = ? AND provider_model = ?", 1, "batch-partial").Count(&cross)
	if cross != 0 {
		t.Fatalf("pairs 不该产生交叉行，实得 %d", cross)
	}

	// 重复配对整批 400
	code, _ = callBatchCreate(t, map[string]any{
		"provider_id": 1,
		"pairs": []map[string]any{
			{"model_id": 1, "provider_model": "batch-a"},
			{"model_id": 1, "provider_model": "batch-a"},
		},
		"defaults": map[string]any{"weight": 1},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("重复配对应 400，实得 %d", code)
	}
}

// TestBatchCreateConcurrentDuplicateClassified：并发同批请求撞唯一索引时，
// 赢家建行、输家报 exists——skip_existing=false 下输家必须是 failed 而不是
// 被静默吞成 skipped。库里最终恰好一行（索引底座 + 分类语义一起钉）。
func TestBatchCreateConcurrentDuplicateClassified(t *testing.T) {
	setupBatchDB(t)

	const racers = 4
	type outcome struct {
		code    int
		created int
		failed  []BatchFailedItem
		skipped []BatchSkippedItem
	}
	outcomes := make([]outcome, racers)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w := doJSON(t, BatchCreateModelProviders, http.MethodPost, "/api/model-providers/batch", map[string]any{
				"model_ids":       []uint{1},
				"provider_id":     1,
				"provider_models": []string{"race-a"},
				"defaults":        map[string]any{"weight": 1},
				"skip_existing":   false,
			}, nil)
			var env struct {
				Code int             `json:"code"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				outcomes[i] = outcome{code: -1}
				return
			}
			var resp batchResp
			_ = json.Unmarshal(env.Data, &resp)
			outcomes[i] = outcome{code: env.Code, created: len(resp.Created), failed: resp.Failed, skipped: resp.Skipped}
		}(i)
	}
	close(start)
	wg.Wait()

	totalCreated := 0
	existsReported := 0
	for _, o := range outcomes {
		if o.code != http.StatusOK {
			t.Fatalf("并发批量都应 200，实得 code=%d", o.code)
		}
		totalCreated += o.created
		for _, f := range o.failed {
			if f.Reason == "exists" {
				existsReported++
			}
		}
	}
	if totalCreated != 1 {
		t.Fatalf("四个并发请求应恰好建 1 行，实得 %d", totalCreated)
	}
	if existsReported != racers-1 {
		t.Fatalf("撞索引的 %d 个输家都应报 exists 失败，实得 %d（skipped=%+v）",
			racers-1, existsReported, outcomes)
	}
	var rows int64
	models.DB.Model(&models.ModelWithProvider{}).Where("provider_model = ?", "race-a").Count(&rows)
	if rows != 1 {
		t.Fatalf("库里应恰 1 行，实得 %d", rows)
	}
}
