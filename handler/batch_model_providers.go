package handler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atopos31/llmio/balancers"
	"github.com/atopos31/llmio/common"
	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/service/modelmeta"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 批量关联端点族（docs/plans/association-batch-operations.md §4.1.1）。
//
// 与单条端点（api.go）的边界是刻意不同的：单条创建/更新一个字段都不补
//（modelmeta_writepath_test.go 钉着这条），批量路径按**三态语义**只补
// 空位——defaults 里六个可空字段（能力×3、价格×3）为 nil 时才允许元数据
// 建议写入，显式值（含显式 false / 显式 0）是用户意志，一律原样落库。
// 服务端补值只认策略的 Enabled / Sources，Overwrite 恒无效——批量请求
// 里没有"用户改过哪些格"的信息，Overwrite 在这里没有可作用的语义。

// batchAssociationLimit 是单次批量创建的行数上限（模型数 × 上游模型数）。
// SQLite 是全库单写者：一个 model_id 一个短事务，500 行以内锁持有时间
// 尚可有界，再大就该分批提交。
const batchAssociationLimit = 500

// 元数据建议的并发与预算。索引就绪时 Suggest 是纯内存读；预算兜的是
// "每条都要走完整归一化匹配"的最坏情况，超预算的条目归入 unfilled 报告，
// 不阻塞创建本身。
const (
	batchAutofillConcurrency = 4
	batchAutofillBudget      = 10 * time.Second
)

// autofill 超预算时 unfilled 里的原因码。与 modelmeta 的四个原因码并列，
// 表示"这次批量把元数据预算用完了"，稍后重试或用补填端点即可。
const reasonAutofillBudget = "autofill_budget_exhausted"

// BatchAssociationDefaults 是批量创建的公共配置。
//
// 字段集合与 ModelWithProviderRequest 一一对应（model_id / provider_id /
// provider_name 三项移到了批量信封上）——handler 测试里有反射用例钉着
// 这份对应，新增字段漏一边会直接红。
//
// 六个可空字段的语义：nil = 留给元数据建议补（补不了就物化成 false / 0
// 并进 unfilled 报告）；非 nil（含显式 false / 0）= 用户意志，不补不改。
type BatchAssociationDefaults struct {
	Weight     int  `json:"weight"`
	WithHeader bool `json:"with_header"`
	Backup     bool `json:"backup"`

	ToolCall         *bool    `json:"tool_call"`
	StructuredOutput *bool    `json:"structured_output"`
	Image            *bool    `json:"image"`
	InputPrice       *float64 `json:"input_price"`
	CacheReadPrice   *float64 `json:"cache_read_price"`
	OutputPrice      *float64 `json:"output_price"`

	CustomerHeaders map[string]string `json:"customer_headers"`
	ExtraBody       map[string]any    `json:"extra_body"`
	Currency        string            `json:"currency"`
	Peak            *models.PeakTerms `json:"peak"`
}

// BatchPair 是「按名挂载」的显式配对：导入向导第二步逐对建立
// (逻辑模型, 上游模型)，不产生笛卡尔积。
type BatchPair struct {
	ModelID       uint   `json:"model_id"`
	ProviderModel string `json:"provider_model"`
}

type BatchModelProviderRequest struct {
	ModelIDs       []uint   `json:"model_ids"`
	ProviderID     uint     `json:"provider_id"`
	ProviderModels []string `json:"provider_models"`
	// Pairs 给了就忽略 model_ids × provider_models（两者别混用）。
	Pairs    []BatchPair              `json:"pairs"`
	Defaults BatchAssociationDefaults `json:"defaults"`
	// SkipExisting 默认 true：整批重交幂等（已创建的报 skipped 而不是失败）。
	SkipExisting *bool `json:"skip_existing"`
	// AutofillMeta 默认 true，且还要策略总开关开着才生效；false 时六个空位
	// 直接物化成 false / 0。
	AutofillMeta *bool `json:"autofill_meta"`
	// DryRun 走同一套查询与展开、零写入：created 里是"将要写入的完整行"
	//（ID=0）。预查结果非最终——提交时仍可能因并发撞成 exists。
	DryRun bool `json:"dry_run"`
}

// BatchAutofillReport 是一条新建行的补值摘要：补了哪几格、值是什么、
// 出自哪个源。计费字段的写入必须让人看得见——"看不见的价格写入"比
// "没写入"更危险。
type BatchAutofillReport struct {
	Source string         `json:"source,omitempty"`
	Filled map[string]any `json:"filled,omitempty"`
}

func (r *BatchAutofillReport) fill(field string, value any, source string) {
	if r.Filled == nil {
		r.Filled = map[string]any{}
	}
	r.Filled[field] = value
	r.Source = source
}

type BatchCreatedItem struct {
	models.ModelWithProvider
	Autofill *BatchAutofillReport `json:"Autofill,omitempty"`
}

type BatchSkippedItem struct {
	ModelID        uint   `json:"model_id"`
	ProviderModel  string `json:"provider_model"`
	Reason         string `json:"reason"`
	ExistingStatus *bool  `json:"existing_status"`
}

type BatchFailedItem struct {
	ModelID       uint   `json:"model_id"`
	ProviderModel string `json:"provider_model"`
	Reason        string `json:"reason"`
}

type BatchUnfilledItem struct {
	ProviderModel string `json:"provider_model"`
	Reason        string `json:"reason"`
}

// BatchCreateResponse 的四个列表**不带 omitempty 且永远非 nil**：空列表
// 序列化成 [] 而不是 null——调用方（前端与 e2e）对"没有"直接取长度，
// null 会让它们在成功响应上炸。
type BatchCreateResponse struct {
	DryRun   bool                `json:"dry_run"`
	Created  []BatchCreatedItem  `json:"created"`
	Skipped  []BatchSkippedItem  `json:"skipped"`
	Failed   []BatchFailedItem   `json:"failed"`
	Unfilled []BatchUnfilledItem `json:"unfilled"`
}

// BatchCreateModelProviders 批量创建关联。
//
// 失败语义按 model_id 分组：一个模型一个事务，事务内单条失败不影响
// 同模型其它行（唯一索引冲突归入 skipped，其它错误归入 failed），
// 事务整体失败则该模型名下的待建行全部转 failed。
func BatchCreateModelProviders(c *gin.Context) {
	var req BatchModelProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		common.BadRequest(c, msg)
		return
	}

	ctx := c.Request.Context()

	provider, err := gorm.G[models.Provider](models.DB).Where("id = ?", req.ProviderID).First(ctx)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			common.BadRequest(c, "provider not found")
			return
		}
		common.InternalServerError(c, "Failed to retrieve provider: "+err.Error())
		return
	}

	var modelCount int64
	plan, orderedModelIDs, distinctPMs, planErr := req.plan()
	if planErr != "" {
		common.BadRequest(c, planErr)
		return
	}
	if err := models.DB.Model(&models.Model{}).Where("id IN ?", orderedModelIDs).Count(&modelCount).Error; err != nil {
		common.InternalServerError(c, "Failed to validate models: "+err.Error())
		return
	}
	if modelCount != int64(len(orderedModelIDs)) {
		common.BadRequest(c, "model_ids contains non-existing model")
		return
	}

	suggestions, unfilled := prepareSuggestions(ctx, provider, distinctPMs, &req.Defaults, req.AutofillMeta)

	resp := BatchCreateResponse{
		DryRun:   req.DryRun,
		Unfilled: unfilled,
		// 空列表也要是 []，不能是 null（见 BatchCreateResponse 的注释）
		Created: []BatchCreatedItem{},
		Skipped: []BatchSkippedItem{},
		Failed:  []BatchFailedItem{},
	}
	if resp.Unfilled == nil {
		resp.Unfilled = []BatchUnfilledItem{}
	}
	skipExisting := req.SkipExisting == nil || *req.SkipExisting

	for _, modelID := range orderedModelIDs {
		pms := plan[modelID]
		existing := map[string]models.ModelWithProvider{}
		rows, err := gorm.G[models.ModelWithProvider](models.DB).
			Where("model_id = ? AND provider_id = ?", modelID, req.ProviderID).
			Where("provider_model IN ?", pms).
			Find(ctx)
		if err != nil {
			for _, pm := range pms {
				resp.Failed = append(resp.Failed, BatchFailedItem{modelID, pm, "db: " + err.Error()})
			}
			continue
		}
		for _, row := range rows {
			existing[row.ProviderModel] = row
		}

		toCreate := make([]models.ModelWithProvider, 0, len(pms))
		reports := make([]*BatchAutofillReport, 0, len(pms))
		for _, pm := range pms {
			if ex, ok := existing[pm]; ok {
				if skipExisting {
					resp.Skipped = append(resp.Skipped, BatchSkippedItem{modelID, pm, "exists", ex.Status})
				} else {
					resp.Failed = append(resp.Failed, BatchFailedItem{modelID, pm, "exists"})
				}
				continue
			}
			row, report := buildAssociationRow(modelID, req.ProviderID, pm, &req.Defaults, suggestions[pm])
			if req.DryRun {
				resp.Created = append(resp.Created, BatchCreatedItem{ModelWithProvider: row, Autofill: report})
				continue
			}
			toCreate = append(toCreate, row)
			reports = append(reports, report)
		}
		if req.DryRun || len(toCreate) == 0 {
			continue
		}

		// 单条失败不回滚同模型其它行；事务本身失败才整组转 failed。
		var createdInTx []BatchCreatedItem
		txErr := models.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for i := range toCreate {
				if err := tx.Create(&toCreate[i]).Error; err != nil {
					if isUniqueViolation(err) {
						// 并发下撞唯一索引：另一个请求刚建了同一条。它的落库
						// 与本事务无关，按 skip 报告并补读当前状态。
						resp.Skipped = append(resp.Skipped, BatchSkippedItem{
							ModelID: modelID, ProviderModel: toCreate[i].ProviderModel, Reason: "exists",
							ExistingStatus: existingStatusOf(ctx, modelID, req.ProviderID, toCreate[i].ProviderModel),
						})
						continue
					}
					resp.Failed = append(resp.Failed, BatchFailedItem{modelID, toCreate[i].ProviderModel, "db: " + err.Error()})
					continue
				}
				createdInTx = append(createdInTx, BatchCreatedItem{ModelWithProvider: toCreate[i], Autofill: reports[i]})
			}
			return nil
		})
		if txErr != nil {
			for _, row := range toCreate {
				resp.Failed = append(resp.Failed, BatchFailedItem{modelID, row.ProviderModel, "db: " + txErr.Error()})
			}
			continue
		}
		resp.Created = append(resp.Created, createdInTx...)
	}

	common.Success(c, resp)
}

func (r *BatchModelProviderRequest) validate() string {
	if r.ProviderID == 0 {
		return "provider_id is required"
	}
	if r.Defaults.Weight < 1 {
		return "defaults.weight must be >= 1"
	}
	switch r.Defaults.Currency {
	case "", "CNY", "USD":
	default:
		return "defaults.currency must be CNY or USD"
	}

	// pairs 形态的校验在 plan() 里做（那里才看得见配对结构）
	if len(r.Pairs) > 0 {
		return ""
	}

	if len(r.ModelIDs) == 0 {
		return "model_ids cannot be empty"
	}
	seenModel := make(map[uint]struct{}, len(r.ModelIDs))
	for _, id := range r.ModelIDs {
		if id == 0 {
			return "model_ids contains invalid value 0"
		}
		if _, ok := seenModel[id]; ok {
			return "duplicated model_id"
		}
		seenModel[id] = struct{}{}
	}

	seenModelName := make(map[string]struct{}, len(r.ProviderModels))
	for i, pm := range r.ProviderModels {
		r.ProviderModels[i] = strings.TrimSpace(pm)
		if r.ProviderModels[i] == "" {
			return "provider_models contains blank name"
		}
		if _, ok := seenModelName[r.ProviderModels[i]]; ok {
			return "duplicated provider_model: " + r.ProviderModels[i]
		}
		seenModelName[r.ProviderModels[i]] = struct{}{}
	}
	if len(r.ProviderModels) == 0 {
		return "provider_models cannot be empty"
	}

	if len(r.ModelIDs)*len(r.ProviderModels) > batchAssociationLimit {
		return "batch exceeds row limit (model_ids × provider_models > 500)"
	}
	return ""
}

// plan 把两种请求形态归一成「按模型分组的待建清单」并完成 pairs 形态的
// 校验：cartesian（model_ids × provider_models）或显式配对（导入向导第
// 二步用，逐对建立、不交叉）。返回分组、有序模型 id、distinct 上游模型名。
func (r *BatchModelProviderRequest) plan() (map[uint][]string, []uint, []string, string) {
	byModel := map[uint][]string{}
	distinctSet := map[string]struct{}{}

	if len(r.Pairs) > 0 {
		if len(r.Pairs) > batchAssociationLimit {
			return nil, nil, nil, "batch exceeds row limit (pairs > 500)"
		}
		seen := map[uint]map[string]struct{}{}
		for _, p := range r.Pairs {
			pm := strings.TrimSpace(p.ProviderModel)
			if p.ModelID == 0 || pm == "" {
				return nil, nil, nil, "pairs contains invalid model_id or blank provider_model"
			}
			if seen[p.ModelID] == nil {
				seen[p.ModelID] = map[string]struct{}{}
			}
			if _, ok := seen[p.ModelID][pm]; ok {
				return nil, nil, nil, "duplicated pair for model " + fmt.Sprint(p.ModelID) + ": " + pm
			}
			seen[p.ModelID][pm] = struct{}{}
			byModel[p.ModelID] = append(byModel[p.ModelID], pm)
			distinctSet[pm] = struct{}{}
		}
	} else {
		for _, id := range r.ModelIDs {
			byModel[id] = r.ProviderModels
		}
		for _, pm := range r.ProviderModels {
			distinctSet[pm] = struct{}{}
		}
	}

	ordered := make([]uint, 0, len(byModel))
	for id := range byModel {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)

	distinct := make([]string, 0, len(distinctSet))
	for pm := range distinctSet {
		distinct = append(distinct, pm)
	}
	slices.Sort(distinct)

	return byModel, ordered, distinct, ""
}

// prepareSuggestions 按 distinct provider_model 取元数据建议（同一建议对
// 多个 model_id 完全复用）。六个三态位全给了显式值时根本不需要查。
func prepareSuggestions(ctx context.Context, provider models.Provider, pms []string, d *BatchAssociationDefaults, autofillMeta *bool) (map[string]modelmeta.Suggestion, []BatchUnfilledItem) {
	needsAutofill := d.ToolCall == nil || d.StructuredOutput == nil || d.Image == nil ||
		d.InputPrice == nil || d.CacheReadPrice == nil || d.OutputPrice == nil
	if !needsAutofill {
		return nil, nil
	}

	policy := modelmeta.LoadPolicy(ctx)
	if !policy.Enabled {
		return nil, nil
	}
	if autofillMeta != nil && !*autofillMeta {
		return nil, nil
	}

	suggestCtx, cancel := context.WithTimeout(ctx, batchAutofillBudget)
	defer cancel()

	manager := modelMetaManager()
	results := make([]modelmeta.Suggestion, len(pms))
	// done 记"这条查询真的跑完了"：预算耗尽后没跑成的条目才归
	// autofill_budget_exhausted，跑完了的照实报数据源给出的原因——把后者
	// 也吞掉的话，"源里没有"会被说成"稍后再试"，两者对用户是两件事。
	done := make([]bool, len(pms))
	var wg sync.WaitGroup
	sem := make(chan struct{}, batchAutofillConcurrency)
	for i, pm := range pms {
		wg.Add(1)
		go func(i int, pm string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-suggestCtx.Done():
				return
			}
			results[i] = manager.Suggest(suggestCtx, modelmeta.Query{
				ProviderType:   provider.Type,
				ProviderConfig: provider.Config,
				ProviderName:   provider.Name,
				ProviderModel:  pm,
			}, policy)
			done[i] = true
		}(i, pm)
	}
	wg.Wait()

	out := make(map[string]modelmeta.Suggestion, len(pms))
	var unfilled []BatchUnfilledItem
	for i, pm := range pms {
		sug := results[i]
		if !done[i] {
			// 没跑成（预算耗尽/没抢到信号量）：稍后重试或走补填端点
			unfilled = append(unfilled, BatchUnfilledItem{pm, reasonAutofillBudget})
			continue
		}
		out[pm] = sug
		if !sug.Matched {
			unfilled = append(unfilled, BatchUnfilledItem{pm, sug.Reason})
		}
	}
	return out, unfilled
}

// buildAssociationRow 把公共配置落到一行关联上，三态位按"显式值 → 建议 →
// 物化默认"的次序取值，并记录建议实际写了哪几格。
func buildAssociationRow(modelID, providerID uint, pm string, d *BatchAssociationDefaults, sug modelmeta.Suggestion) (models.ModelWithProvider, *BatchAutofillReport) {
	row := models.ModelWithProvider{
		ModelID:         modelID,
		ProviderID:      providerID,
		ProviderModel:   pm,
		Weight:          d.Weight,
		WithHeader:      &d.WithHeader,
		Backup:          &d.Backup,
		CustomerHeaders: orEmptyHeaders(d.CustomerHeaders),
		ExtraBody:       orEmptyBody(d.ExtraBody),
		Peak:            d.Peak,
	}
	status := true
	row.Status = &status

	var report *BatchAutofillReport
	if sug.Matched {
		report = &BatchAutofillReport{}
	}

	resolveBool := func(explicit *bool, fromSug *bool, field string) *bool {
		if explicit != nil {
			return explicit
		}
		if sug.Matched && fromSug != nil {
			report.fill(field, *fromSug, sug.Source)
			return fromSug
		}
		return new(false)
	}
	resolvePrice := func(explicit *float64, fromSug *float64, field string) *float64 {
		if explicit != nil {
			return explicit
		}
		if sug.Matched && fromSug != nil {
			report.fill(field, *fromSug, sug.Source)
			return fromSug
		}
		zero := 0.0
		return &zero
	}

	row.ToolCall = resolveBool(d.ToolCall, sug.ToolCall, "tool_call")
	row.StructuredOutput = resolveBool(d.StructuredOutput, sug.StructuredOutput, "structured_output")
	row.Image = resolveBool(d.Image, sug.Image, "image")
	row.InputPrice = resolvePrice(d.InputPrice, sug.InputPrice, "input_price")
	row.CacheReadPrice = resolvePrice(d.CacheReadPrice, sug.CacheReadPrice, "cache_read_price")
	row.OutputPrice = resolvePrice(d.OutputPrice, sug.OutputPrice, "output_price")

	// 币种与价格是不可拆的补填单元：显式给过任一档价格，币种跟 defaults；
	// 价格全部来自建议，币种跟建议（源均为 USD/百万 token 口径）；两者
	// 皆无时空值兜底 CNY，对齐 init.go 的回填口径。
	explicitPrice := d.InputPrice != nil || d.CacheReadPrice != nil || d.OutputPrice != nil
	switch {
	case explicitPrice:
		row.Currency = d.Currency
	case report != nil && report.Filled != nil && (report.Filled["input_price"] != nil || report.Filled["cache_read_price"] != nil || report.Filled["output_price"] != nil):
		row.Currency = sug.Currency
	default:
		row.Currency = d.Currency
	}
	if row.Currency == "" {
		row.Currency = "CNY"
	}

	if report != nil && report.Filled == nil {
		report = nil
	}
	return row, report
}

func orEmptyHeaders(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func orEmptyBody(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

func isUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}

func existingStatusOf(ctx context.Context, modelID, providerID uint, pm string) *bool {
	row, err := gorm.G[models.ModelWithProvider](models.DB).
		Where("model_id = ? AND provider_id = ? AND provider_model = ?", modelID, providerID, pm).
		First(ctx)
	if err != nil {
		return nil
	}
	return row.Status
}

// ── 批量启停与删除 ─────────────────────────────────────────────────────────

type BatchProviderStatusRequest struct {
	IDs    []uint `json:"ids"`
	Status bool   `json:"status"`
}

type BatchIDsRequest struct {
	IDs []uint `json:"ids"`
}

type BatchMutationResponse struct {
	// Updated / Deleted 是成功的 id；NotFound 是不存在的 id——幂等重交时
	// 它们不该算失败。三个键永远在场（空为 []），调用方不用判空。
	Updated  []uint `json:"updated"`
	Deleted  []uint `json:"deleted"`
	NotFound []uint `json:"not_found"`
}

// batchMutationIDsLimit 是启停/删除/补填的 ids 上限。SQLite 绑定参数上限
// 32766，超了直接驱动层报错（500）；全选大列表的场景在这里就按住。
const batchMutationIDsLimit = 1000

func validateBatchIDs(ids []uint) string {
	if len(ids) == 0 {
		return "ids cannot be empty"
	}
	if len(ids) > batchMutationIDsLimit {
		return "ids exceeds limit (1000)"
	}
	seen := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return "ids contains invalid value 0"
		}
		if _, ok := seen[id]; ok {
			return "duplicated id"
		}
		seen[id] = struct{}{}
	}
	return ""
}

// BatchUpdateModelProviderStatus 批量启停。不存在的 id 计入 not_found，
// 不让整批失败——幂等重交的前提。
func BatchUpdateModelProviderStatus(c *gin.Context) {
	var req BatchProviderStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	if msg := validateBatchIDs(req.IDs); msg != "" {
		common.BadRequest(c, msg)
		return
	}

	ctx := c.Request.Context()
	rows, err := gorm.G[models.ModelWithProvider](models.DB).Where("id IN ?", req.IDs).Find(ctx)
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve associations: "+err.Error())
		return
	}

	found := make(map[uint]struct{}, len(rows))
	for _, row := range rows {
		found[row.ID] = struct{}{}
	}

	resp := BatchMutationResponse{Updated: []uint{}, Deleted: []uint{}, NotFound: []uint{}}
	for _, id := range req.IDs {
		if _, ok := found[id]; !ok {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	if len(rows) > 0 {
		err := models.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, row := range rows {
				if err := tx.Model(&models.ModelWithProvider{}).
					Where("id = ?", row.ID).
					Update("status", req.Status).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			common.InternalServerError(c, "Failed to update status: "+err.Error())
			return
		}
		for _, row := range rows {
			resp.Updated = append(resp.Updated, row.ID)
		}
	}

	common.Success(c, resp)
}

// BatchDeleteModelProviders 批量删除（软删）。删除成功后同步清掉熔断器
// 里的节点——nodes 是只增不减的包级表，批量删除的 churn 会让它无界增长。
func BatchDeleteModelProviders(c *gin.Context) {
	var req BatchIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	if msg := validateBatchIDs(req.IDs); msg != "" {
		common.BadRequest(c, msg)
		return
	}

	ctx := c.Request.Context()
	rows, err := gorm.G[models.ModelWithProvider](models.DB).Where("id IN ?", req.IDs).Find(ctx)
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve associations: "+err.Error())
		return
	}

	found := make(map[uint]struct{}, len(rows))
	for _, row := range rows {
		found[row.ID] = struct{}{}
	}

	resp := BatchMutationResponse{Updated: []uint{}, Deleted: []uint{}, NotFound: []uint{}}
	for _, id := range req.IDs {
		if _, ok := found[id]; !ok {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	if len(rows) > 0 {
		err := models.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, row := range rows {
				if err := tx.Delete(&models.ModelWithProvider{}, row.ID).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			common.InternalServerError(c, "Failed to delete associations: "+err.Error())
			return
		}
		for _, row := range rows {
			resp.Deleted = append(resp.Deleted, row.ID)
			// 熔断状态跟着关联行走：行没了，节点留着只会让下一次
			// BalancerWrapperBreaker 的全表扫描白走一段。
			balancers.Forget(row.ID)
		}
	}

	common.Success(c, resp)
}
