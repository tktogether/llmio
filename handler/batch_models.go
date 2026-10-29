package handler

import (
	"strings"

	"github.com/atopos31/llmio/common"
	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/service/modelmeta"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 「从供应商导入」的两步后端（方案 §4.2.1）：
//
//	POST /api/models/batch                 批量建逻辑模型（幂等：同名跳过）
//	POST /api/model-providers/batch/backfill  对已存在关联补填空位元数据
//
// 刻意不做"一步到位的 batch-import"：一次调用同时建两个聚合根（Model +
// ModelWithProvider）是最重的端点语义，拆成两步后每一步都单语义、可独立
// 幂等重试，前端向导负责编排。

// batchNamesLimit 与关联批量同额：一次向导别超过这个量级。
const batchNamesLimit = 500

// 批量建逻辑模型的服务端默认值。
//
// 单条 CreateModel 只补 strategy（历史行为，writepath 测试钉着"一个字段
// 都不补"），time_out=0/max_retry=0 落库后是"必超时/零重试"的毒药——批量
// 导入会把这个毒药放大 N 倍，所以批量路径定义显式默认值，且拒绝 <1。
const (
	batchModelDefaultTimeOut  = 300
	batchModelDefaultMaxRetry = 3
)

type BatchModelDefaults struct {
	Strategy     *string `json:"strategy"`
	MaxRetry     *int    `json:"max_retry"`
	TimeOut      *int    `json:"time_out"`
	Breaker      *bool   `json:"breaker"`
	PreferDirect *bool   `json:"prefer_direct"`
}

type BatchModelsRequest struct {
	Names    []string            `json:"names"`
	Defaults *BatchModelDefaults `json:"defaults"`
}

type BatchSkippedName struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type BatchModelsResponse struct {
	Created []models.Model     `json:"created"`
	Skipped []BatchSkippedName `json:"skipped"`
}

// BatchCreateModels 批量建逻辑模型。
//
// 数值字段全是指针：nil 落服务端默认，显式值（会被校验）原样——批量请求
// 里"没传"与"传了 0"必须可区分，0 在这套字段上是毒药（立即超时/零重试）。
func BatchCreateModels(c *gin.Context) {
	var req BatchModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Names) == 0 {
		common.BadRequest(c, "names cannot be empty")
		return
	}
	if len(req.Names) > batchNamesLimit {
		common.BadRequest(c, "names exceeds limit (500)")
		return
	}
	seen := make(map[string]struct{}, len(req.Names))
	for i, name := range req.Names {
		req.Names[i] = strings.TrimSpace(name)
		if req.Names[i] == "" {
			common.BadRequest(c, "names contains blank name")
			return
		}
		if _, ok := seen[req.Names[i]]; ok {
			common.BadRequest(c, "duplicated name: "+req.Names[i])
			return
		}
		seen[req.Names[i]] = struct{}{}
	}

	d := req.Defaults
	if d == nil {
		d = &BatchModelDefaults{}
	}
	maxRetry := batchModelDefaultMaxRetry
	if d.MaxRetry != nil {
		if *d.MaxRetry < 1 {
			common.BadRequest(c, "defaults.max_retry must be >= 1")
			return
		}
		maxRetry = *d.MaxRetry
	}
	timeOut := batchModelDefaultTimeOut
	if d.TimeOut != nil {
		if *d.TimeOut < 1 {
			common.BadRequest(c, "defaults.time_out must be >= 1")
			return
		}
		timeOut = *d.TimeOut
	}
	strategy := consts.BalancerDefault
	if d.Strategy != nil && *d.Strategy != "" {
		if *d.Strategy != consts.BalancerLottery && *d.Strategy != consts.BalancerRotor {
			common.BadRequest(c, "defaults.strategy must be lottery or rotor")
			return
		}
		strategy = *d.Strategy
	}

	ctx := c.Request.Context()
	resp := BatchModelsResponse{
		Created: []models.Model{},
		Skipped: []BatchSkippedName{},
	}

	// 整批一个事务：Model 行小，锁持有时间可忽略；display_order 在事务内
	// 一次取 MAX 后自增，消掉逐行 MAX 的竞态。
	err := models.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var maxOrder int
		if err := tx.Model(&models.Model{}).
			Select("COALESCE(MAX(display_order), 0)").Scan(&maxOrder).Error; err != nil {
			return err
		}
		for _, name := range req.Names {
			var count int64
			if err := tx.Model(&models.Model{}).
				Where("name = ?", name).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				resp.Skipped = append(resp.Skipped, BatchSkippedName{Name: name, Reason: "exists"})
				continue
			}
			maxOrder++
			row := models.Model{
				Name:         name,
				MaxRetry:     maxRetry,
				TimeOut:      timeOut,
				Strategy:     strategy,
				Breaker:      d.Breaker,
				PreferDirect: d.PreferDirect,
				DisplayOrder: maxOrder,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			resp.Created = append(resp.Created, row)
		}
		return nil
	})
	if err != nil {
		common.InternalServerError(c, "Failed to create models: "+err.Error())
		return
	}

	common.Success(c, resp)
}

// ── 元数据补填 ─────────────────────────────────────────────────────────────

type BackfillRequest struct {
	IDs []uint `json:"ids"`
}

type BackfillFilledItem struct {
	ID     uint           `json:"id"`
	Source string         `json:"source,omitempty"`
	Fields map[string]any `json:"fields"`
}

type BackfillUnfilledItem struct {
	ID            uint   `json:"id"`
	ProviderModel string `json:"provider_model"`
	Reason        string `json:"reason"`
}

type BackfillResponse struct {
	Filled    []BackfillFilledItem   `json:"filled"`
	Unchanged []uint                 `json:"unchanged"`
	NotFound  []uint                 `json:"not_found"`
	Unfilled  []BackfillUnfilledItem `json:"unfilled"`
}

// BatchBackfillMetadata 对已存在关联补填空位元数据。
//
// 这是 autofill 冷启动（首次抓数据源要 5MB）期间批量导入的正规出口：那批
// 落了 0 价 / false 能力的行，等 catalog 就绪后在这里一次补齐。
//
// 两种"空位"的判定不同，必须分开说：
//   - 能力位：列本来就是指针，nil 就是"从没配过"，补它没有歧义；
//   - 价格位：批量创建时"补不了"已经物化成 0，与"用户就要免费"在行上
//     不可区分。这里选择**补 0**（0 价在计费上等于没配价），并把补了什么
//     逐字段报告回来——显式要 0 价的行别对它跑补填，或看报告后改回。
//
// 只认策略的 Enabled/Sources；Overwrite 恒无效（与批量创建同一边界，见
// modelmeta 包注释）。
func BatchBackfillMetadata(c *gin.Context) {
	var req BackfillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	if msg := validateBatchIDs(req.IDs); msg != "" {
		common.BadRequest(c, msg)
		return
	}

	ctx := c.Request.Context()
	policy := modelmeta.LoadPolicy(ctx)
	if !policy.Enabled {
		common.BadRequest(c, "autofill policy is disabled")
		return
	}

	rows, err := gorm.G[models.ModelWithProvider](models.DB).Where("id IN ?", req.IDs).Find(ctx)
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve associations: "+err.Error())
		return
	}
	found := make(map[uint]models.ModelWithProvider, len(rows))
	for _, row := range rows {
		found[row.ID] = row
	}
	resp := BackfillResponse{
		Filled:    []BackfillFilledItem{},
		Unchanged: []uint{},
		NotFound:  []uint{},
		Unfilled:  []BackfillUnfilledItem{},
	}
	for _, id := range req.IDs {
		if _, ok := found[id]; !ok {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	if len(rows) == 0 {
		common.Success(c, resp)
		return
	}

	providerIDs := make(map[uint]struct{}, len(rows))
	for _, row := range rows {
		providerIDs[row.ProviderID] = struct{}{}
	}
	providers, err := gorm.G[models.Provider](models.DB).Where("id IN ?", keysOf(providerIDs)).Find(ctx)
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve providers: "+err.Error())
		return
	}
	providerByID := make(map[uint]models.Provider, len(providers))
	for _, p := range providers {
		providerByID[p.ID] = p
	}

	// 同一 (provider, provider_model) 的建议对多行完全复用
	type pmKey struct {
		providerID uint
		pm         string
	}
	needSuggest := make(map[pmKey]struct{})
	for _, row := range rows {
		needSuggest[pmKey{row.ProviderID, row.ProviderModel}] = struct{}{}
	}
	manager := modelMetaManager()
	suggestions := make(map[pmKey]modelmeta.Suggestion, len(needSuggest))
	missingProvider := make(map[pmKey]bool)
	for key := range needSuggest {
		p, ok := providerByID[key.providerID]
		if !ok {
			// 供应商行没了（被删）：给个说得出口的原因码，别让前端渲染空串
			missingProvider[key] = true
			continue
		}
		suggestions[key] = manager.Suggest(ctx, modelmeta.Query{
			ProviderType:   p.Type,
			ProviderConfig: p.Config,
			ProviderName:   p.Name,
			ProviderModel:  key.pm,
		}, policy)
	}

	for _, row := range rows {
		key := pmKey{row.ProviderID, row.ProviderModel}
		if missingProvider[key] {
			resp.Unfilled = append(resp.Unfilled, BackfillUnfilledItem{row.ID, row.ProviderModel, "provider_not_found"})
			continue
		}
		sug := suggestions[key]
		if !sug.Matched {
			resp.Unfilled = append(resp.Unfilled, BackfillUnfilledItem{row.ID, row.ProviderModel, sug.Reason})
			continue
		}

		updates := map[string]any{}
		report := BackfillFilledItem{ID: row.ID, Source: sug.Source, Fields: map[string]any{}}

		fillCap := func(current *bool, fromSug *bool, column, field string) {
			if current == nil && fromSug != nil {
				updates[column] = *fromSug
				report.Fields[field] = *fromSug
			}
		}
		fillCap(row.ToolCall, sug.ToolCall, "tool_call", "tool_call")
		fillCap(row.StructuredOutput, sug.StructuredOutput, "structured_output", "structured_output")
		fillCap(row.Image, sug.Image, "image", "image")

		// 价格补 0：见函数头注释的取舍
		fillPrice := func(current *float64, fromSug *float64, column, field string) {
			if (current == nil || *current == 0) && fromSug != nil && *fromSug != 0 {
				updates[column] = *fromSug
				report.Fields[field] = *fromSug
			}
		}
		fillPrice(row.InputPrice, sug.InputPrice, "input_price", "input_price")
		fillPrice(row.CacheReadPrice, sug.CacheReadPrice, "cache_read_price", "cache_read_price")
		fillPrice(row.OutputPrice, sug.OutputPrice, "output_price", "output_price")

		if len(updates) == 0 {
			resp.Unchanged = append(resp.Unchanged, row.ID)
			continue
		}

		// 币种联动：只要补进了任一档价格，币种跟建议（源均为 USD/百万 token）
		priceFilled := false
		for _, f := range []string{"input_price", "cache_read_price", "output_price"} {
			if _, ok := report.Fields[f]; ok {
				priceFilled = true
				break
			}
		}
		if priceFilled && sug.Currency != "" && row.Currency != sug.Currency {
			updates["currency"] = sug.Currency
			report.Fields["currency"] = sug.Currency
		}

		if err := models.DB.WithContext(ctx).
			Model(&models.ModelWithProvider{}).
			Where("id = ?", row.ID).
			Updates(updates).Error; err != nil {
			resp.Unfilled = append(resp.Unfilled, BackfillUnfilledItem{row.ID, row.ProviderModel, "db: " + err.Error()})
			continue
		}
		resp.Filled = append(resp.Filled, report)
	}

	common.Success(c, resp)
}

func keysOf(set map[uint]struct{}) []uint {
	out := make([]uint, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
