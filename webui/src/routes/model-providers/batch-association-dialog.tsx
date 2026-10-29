import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Search, X } from "lucide-react";
import {
  batchCreateModelProviders,
  type BatchCreateResult,
  type BatchCreatedItem,
  type BatchModelProviderRequest,
} from "@/lib/api";
import type { Model, Provider, ProviderModel } from "@/lib/api";

/**
 * 批量关联弹窗。
 *
 * 单条弹窗（model-provider-form-dialog）一个字段都不动——批量是另一个入口，
 * 不是单条的替代。两种模式：
 *
 * - 同名挂载（默认）：选一批上游模型，自动挂到**同名**逻辑模型上。这是
 *   #31/#51 反馈的真实诉求（"把它挂到那 20 个模型上"），也是 one-api 系
 *   "渠道贴模型串"天然同名对齐的形态。
 * - 指定挂载（高级，显式切换）：逻辑模型集合 × 上游模型集合的笛卡尔积。
 *   预览里非同名的组合会高亮——错建一片的主要来源就是没看见这一点。
 *
 * 公共配置的六个元数据位是三态：留空 = 交给服务端按数据源补（补不了落
 * false/0 并在结果里报告）；显式值（含 0 与"关"）是用户意志，一律不补。
 */

type CapabilityTriState = "auto" | "on" | "off";

type BatchMode = "same-name" | "assign";

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  models: Model[];
  providers: Provider[];
  providerModelsMap: Record<number, ProviderModel[]>;
  providerModelsLoading: Record<number, boolean>;
  loadProviderModels: (providerId: number, force?: boolean) => Promise<void>;
  /** 从模型详情页打开时锁定该模型，且默认指定挂载模式 */
  lockedModelId?: number | null;
  onCompleted: (touchedModelIds: number[]) => void;
};

const emptyDefaults = {
  weight: 1,
  with_header: false,
  backup: false,
  /** "auto" 是"留空交给补全"的哨兵：Radix Select 不允许空串 value，
   *  提交时映射回空串（后端按未指定处理）。 */
  currency: "auto",
  tool_call: "auto" as CapabilityTriState,
  structured_output: "auto" as CapabilityTriState,
  image: "auto" as CapabilityTriState,
  input_price: "",
  cache_read_price: "",
  output_price: "",
};

export function BatchAssociationDialog({
  open,
  onOpenChange,
  models,
  providers,
  providerModelsMap,
  providerModelsLoading,
  loadProviderModels,
  lockedModelId,
  onCompleted,
}: Props) {
  const { t } = useTranslation(["models", "common"]);
  const [mode, setMode] = useState<BatchMode>(lockedModelId ? "assign" : "same-name");
  const [providerId, setProviderId] = useState(0);
  const [pmSearch, setPmSearch] = useState("");
  const [selectedPMs, setSelectedPMs] = useState<Set<string>>(new Set());
  const [manualName, setManualName] = useState("");
  const [pasteMode, setPasteMode] = useState(false);
  const [pasteText, setPasteText] = useState("");
  const [selectedModelIds, setSelectedModelIds] = useState<Set<number>>(new Set());
  const [modelSearch, setModelSearch] = useState("");
  const [defaults, setDefaults] = useState(emptyDefaults);
  const [preview, setPreview] = useState<BatchCreateResult | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [result, setResult] = useState<BatchCreateResult | null>(null);
  const [submitting, setSubmitting] = useState(false);
  /** 预览对应的提交体快照：确认时原样重放，避免"预览的是 A、提交的是 B" */
  const pendingRef = useRef<BatchModelProviderRequest | null>(null);
  /** 最近一次**真实提交**的请求体：重试从它过滤，不吃表单后续编辑 */
  const lastSubmitRef = useRef<BatchModelProviderRequest | null>(null);
  /** 提交成功后已即时通知页面刷新：结果面板在场时数据就已是最新的，
   *  关闭时的兜底通知跳过，避免同一批数据拉两遍。 */
  const refreshedRef = useRef(false);

  // 打开时重置；锁定模型时默认选中它
  useEffect(() => {
    if (!open) return;
    setMode(lockedModelId ? "assign" : "same-name");
    setProviderId(0);
    setSelectedPMs(new Set());
    setSelectedModelIds(lockedModelId ? new Set([lockedModelId]) : new Set());
    setDefaults(emptyDefaults);
    setPreview(null);
    setResult(null);
    pendingRef.current = null;
    lastSubmitRef.current = null;
    refreshedRef.current = false;
    setPmSearch("");
    setModelSearch("");
    setManualName("");
    setPasteMode(false);
    setPasteText("");
  }, [open, lockedModelId]);

  useEffect(() => {
    if (open && providerId > 0) {
      void loadProviderModels(providerId);
    }
  }, [open, providerId, loadProviderModels]);

  // 预览随表单失效：任何进请求体的状态变了，旧预览就成了"改之前的答案"，
  // 确认按钮必须跟着消失——否则点确认提交的是旧配置（对照条款预览的同一纪律）。
  useEffect(() => {
    setPreview(null);
    pendingRef.current = null;
  }, [mode, selectedPMs, selectedModelIds, defaults]);

  const providerModels = useMemo(
    () => (providerId ? providerModelsMap[providerId] || [] : []),
    [providerId, providerModelsMap],
  );
  const providerModelsLoaded = providerId > 0 && providerModelsMap[providerId] !== undefined;
  const modelByName = useMemo(() => new Map(models.map((m) => [m.Name, m])), [models]);
  const modelById = useMemo(() => new Map(models.map((m) => [m.ID, m])), [models]);

  // 同名挂载：上游模型名 → 同名逻辑模型。没有同名的在此就看得见，
  // 不用等提交后被静默跳过。
  const sameNamePairs = useMemo(() => {
    const pairs: { pm: string; model: Model | null }[] = [];
    for (const pm of selectedPMs) {
      pairs.push({ pm, model: modelByName.get(pm) ?? null });
    }
    return pairs.sort((a, b) => a.pm.localeCompare(b.pm));
  }, [selectedPMs, modelByName]);

  const filteredProviderModels = useMemo(() => {
    const q = pmSearch.trim().toLowerCase();
    if (!q) return providerModels;
    return providerModels.filter((m) => m.id.toLowerCase().includes(q));
  }, [providerModels, pmSearch]);

  const filteredModels = useMemo(() => {
    const q = modelSearch.trim().toLowerCase();
    if (!q) return models;
    return models.filter((m) => m.Name.toLowerCase().includes(q));
  }, [models, modelSearch]);

  // 选区里的名字不在上游列表中的（手动添加 / 粘贴进来的）：以 chips 呈现，
  // 不然它们"看不见却生效"，是最典型的误操作来源。
  const manualPMs = useMemo(
    () => [...selectedPMs].filter((pm) => !providerModels.some((m) => m.id === pm)),
    [selectedPMs, providerModels],
  );

  const effectiveModelIds = useMemo(() => {
    if (mode === "same-name") {
      return sameNamePairs.filter((p) => p.model).map((p) => p.model!.ID);
    }
    return [...selectedModelIds];
  }, [mode, sameNamePairs, selectedModelIds]);

  const effectivePMs = useMemo(() => {
    if (mode === "same-name") {
      return sameNamePairs.filter((p) => p.model).map((p) => p.pm);
    }
    return [...selectedPMs];
  }, [mode, sameNamePairs, selectedPMs]);

  // 价格输入是否可解析（空串=合法的"留空"）。非法值（如 "-"、"1.2.3"）
  // 不按住预览入口的话，Number() 会得到 NaN、JSON 序列化落 null——
  // "手滑打了个半个数字"就这么静默变成"交给补全"，是最阴的误操作。
  const priceInvalid =
    [defaults.input_price, defaults.cache_read_price, defaults.output_price].some(
      (v) => v.trim() !== "" && Number.isNaN(Number(v)),
    );

  const buildRequest = (dryRun: boolean): BatchModelProviderRequest | null => {
    if (!providerId || effectiveModelIds.length === 0 || effectivePMs.length === 0) return null;
    if (priceInvalid) return null;
    const price = (v: string) => (v.trim() === "" ? null : Number(v));
    const cap = (v: CapabilityTriState) => (v === "auto" ? null : v === "on");
    // 同名挂载走 pairs：恒等映射逐对建立。发 model_ids × provider_models
    // 会被服务端笛卡尔展开——N 个同名模型建出 N×N 条、其中 N²−N 条交叉行。
    const pairs =
      mode === "same-name"
        ? sameNamePairs
            .filter((p) => p.model)
            .map((p) => ({ model_id: p.model!.ID, provider_model: p.pm }))
        : undefined;
    return {
      model_ids: pairs ? [] : effectiveModelIds,
      provider_id: providerId,
      provider_models: pairs ? [] : effectivePMs,
      ...(pairs ? { pairs } : {}),
      defaults: {
        weight: defaults.weight,
        with_header: defaults.with_header,
        backup: defaults.backup,
        tool_call: cap(defaults.tool_call),
        structured_output: cap(defaults.structured_output),
        image: cap(defaults.image),
        input_price: price(defaults.input_price),
        cache_read_price: price(defaults.cache_read_price),
        output_price: price(defaults.output_price),
        currency: defaults.currency === "auto" ? "" : defaults.currency,
      },
      dry_run: dryRun,
    };
  };

  const runBatch = async (req: BatchModelProviderRequest) => {
    const res = await batchCreateModelProviders(req);
    if (req.dry_run) {
      setPreview(res);
      pendingRef.current = req;
    } else {
      setResult(res);
      setPreview(null);
      pendingRef.current = null;
      // 重试的基底是"这次真正提交了什么"，不是表单当前的样子——结果面板
      // 在场时表单仍可编辑，现场重建会带着与首批不一致的配置落库。
      lastSubmitRef.current = req;
      // 关联已落库就立刻刷新页面数据：用户停在结果面板时总览计数与关联
      // 列表就该是最新的，不该等关掉弹窗才发现数字没动。
      notifyRefresh(res.created);
    }
    return res;
  };

  const notifyRefresh = (created: BatchCreatedItem[]) => {
    refreshedRef.current = true;
    void onCompleted([...new Set([...created.map((c) => c.ModelID), ...effectiveModelIds])]);
  };

  const handlePreview = async () => {
    const req = buildRequest(true);
    if (!req) return;
    setPreviewing(true);
    setPreview(null);
    try {
      await runBatch({ ...req, dry_run: true });
    } catch (err) {
      toast.error(t("batch_association.preview_failed", { message: err instanceof Error ? err.message : String(err) }));
    } finally {
      setPreviewing(false);
    }
  };

  const handleConfirm = async () => {
    const req = pendingRef.current;
    if (!req) return;
    setSubmitting(true);
    try {
      await runBatch({ ...req, dry_run: false });
    } catch (err) {
      toast.error(t("batch_association.submit_failed", { message: err instanceof Error ? err.message : String(err) }));
    } finally {
      setSubmitting(false);
    }
  };

  /** 只重交失败子集：失败项本身就是精确的 (model_id, provider_model) 对，
   *  直接以 pairs 形态重发——不重算笛卡尔，也不带表单后续的改动。 */
  const handleRetryFailed = async () => {
    if (!result || result.failed.length === 0) return;
    const base = lastSubmitRef.current;
    if (!base) return;
    const req: BatchModelProviderRequest = {
      ...base,
      dry_run: false,
      model_ids: [],
      provider_models: [],
      pairs: result.failed.map((f) => ({ model_id: f.model_id, provider_model: f.provider_model })),
    };
    setSubmitting(true);
    try {
      const retried = await batchCreateModelProviders(req);
      // 合并：重试那部分的结果替换掉旧的 failed，其余原样保留；
      // unfilled 按 pm+reason 去重（跨重试同一原因不重复刷屏）
      setResult({
        dry_run: false,
        created: [...result.created, ...retried.created],
        skipped: [...result.skipped, ...retried.skipped],
        failed: retried.failed,
        unfilled: [
          ...new Map(
            [...result.unfilled, ...retried.unfilled].map((u) => [`${u.provider_model}\u0000${u.reason}`, u]),
          ).values(),
        ],
      });
      lastSubmitRef.current = req;
      // 重试新增的行同样即时上报（幂等刷新，代价是几次重复拉取）
      notifyRefresh([...result.created, ...retried.created]);
    } catch (err) {
      toast.error(t("batch_association.submit_failed", { message: err instanceof Error ? err.message : String(err) }));
    } finally {
      setSubmitting(false);
    }
  };

  const handleClose = () => {
    // 提交时已即时刷新过的不再补拉；仅"有改动但还没通知过"（理论不可达，
    // 留作兜底）才在关闭时刷新
    if (result && !refreshedRef.current && (result.created.length > 0 || result.skipped.length > 0)) {
      onCompleted([...new Set([...result.created.map((c) => c.ModelID), ...effectiveModelIds])]);
    }
    onOpenChange(false);
  };

  const togglePM = (pm: string) => {
    setSelectedPMs((prev) => {
      const next = new Set(prev);
      if (next.has(pm)) next.delete(pm);
      else next.add(pm);
      return next;
    });
  };

  const allVisibleSelected = filteredProviderModels.length > 0 &&
    filteredProviderModels.every((m) => selectedPMs.has(m.id));

  const toggleSelectAll = () => {
    setSelectedPMs((prev) => {
      const next = new Set(prev);
      if (allVisibleSelected) {
        for (const m of filteredProviderModels) next.delete(m.id);
      } else {
        for (const m of filteredProviderModels) next.add(m.id);
      }
      return next;
    });
  };

  const applyPaste = (text: string) => {
    setPasteText(text);
    const names = text
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter(Boolean);
    setSelectedPMs(new Set(names));
  };

  // 粘贴模式进入时以当前选区初始化文本，保证两个方向的同步起点一致
  useEffect(() => {
    if (pasteMode) {
      setPasteText([...selectedPMs].sort().join("\n"));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pasteMode]);

  // weight<1 会被后端整批 400，这里先按住预览入口；价格非法同理
  //（NaN→null 会把半成品输入静默变成"交给补全"）
  const canPreview =
    providerId > 0 &&
    effectiveModelIds.length > 0 &&
    effectivePMs.length > 0 &&
    defaults.weight >= 1 &&
    !priceInvalid;

  const reasonText = (reason: string) =>
    t(`batch_association.reasons.${reason}`, { defaultValue: reason });

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) handleClose(); }}>
      <DialogContent className="max-h-[85vh] flex flex-col" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{t("batch_association.title")}</DialogTitle>
          <DialogDescription>{t("batch_association.desc")}</DialogDescription>
        </DialogHeader>

        <div className="space-y-4 overflow-y-auto pr-1 flex-1 min-h-0">
          {/* 模式（锁定单模型时隐藏——指定挂载且只有一个模型没有歧义） */}
          {!lockedModelId && (
            <div className="grid grid-cols-2 gap-2">
              {(["same-name", "assign"] as const).map((m) => (
                <button
                  key={m}
                  type="button"
                  onClick={() => setMode(m)}
                  className={`rounded-md border p-3 text-left ${mode === m ? "border-primary bg-primary/5" : "border-border hover:bg-accent"}`}
                >
                  <p className="text-sm font-medium">{t(`batch_association.mode_${m}`)}</p>
                  <p className="text-xs text-muted-foreground mt-1">{t(`batch_association.mode_${m}_desc`)}</p>
                </button>
              ))}
            </div>
          )}

          {/* 供应商 */}
          <div className="flex flex-col gap-1 text-xs">
            <Label>{t("batch_association.provider")}</Label>
            <Select value={providerId ? providerId.toString() : ""} onValueChange={(v) => { setProviderId(Number(v)); setSelectedPMs(new Set()); setPreview(null); }}>
              <SelectTrigger className="h-8 text-xs px-2">
                <SelectValue placeholder={t("batch_association.provider_placeholder")} />
              </SelectTrigger>
              <SelectContent>
                {providers.map((p) => (
                  <SelectItem key={p.ID} value={p.ID.toString()}>{p.Name}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {/* 上游模型多选 */}
          {providerId > 0 && (
            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <Label className="text-xs">
                  {t("batch_association.provider_models")}
                  <span className="ml-2 text-muted-foreground">
                    {t("batch_association.selected_count", { count: selectedPMs.size })}
                  </span>
                </Label>
                <div className="flex items-center gap-1">
                  {!pasteMode ? (
                    <>
                      <Button type="button" variant="ghost" size="sm" onClick={toggleSelectAll} disabled={providerModels.length === 0}>
                        {allVisibleSelected ? t("batch_association.unselect_all") : t("batch_association.select_all")}
                      </Button>
                      <Button type="button" variant="ghost" size="sm" onClick={() => setPasteMode(true)}>
                        {t("batch_association.paste_mode")}
                      </Button>
                    </>
                  ) : (
                    <Button type="button" variant="ghost" size="sm" onClick={() => setPasteMode(false)}>
                      {t("batch_association.list_mode")}
                    </Button>
                  )}
                </div>
              </div>

              {pasteMode ? (
                <textarea
                  aria-label={t("batch_association.paste_placeholder")}
                  className="flex min-h-[120px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm font-mono focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={pasteText}
                  onChange={(e) => applyPaste(e.target.value)}
                  placeholder={t("batch_association.paste_placeholder")}
                />
              ) : (
                <>
                  <div className="relative">
                    <Search className="size-3.5 absolute left-2 top-1/2 -translate-y-1/2 text-muted-foreground" />
                    <Input
                      className="h-8 pl-7 text-xs"
                      placeholder={t("batch_association.pm_search_placeholder")}
                      value={pmSearch}
                      onChange={(e) => setPmSearch(e.target.value)}
                    />
                  </div>
                  {providerModelsLoading[providerId] ? (
                    <div className="flex items-center gap-2 text-xs text-muted-foreground py-4">
                      <Spinner className="size-4" />{t("batch_association.loading_models")}
                    </div>
                  ) : providerModelsLoaded && providerModels.length === 0 ? (
                    <div className="text-xs text-muted-foreground py-2">
                      {t("batch_association.no_models_fetched")}
                    </div>
                  ) : (
                    <div className="max-h-56 overflow-y-auto rounded-md border">
                      {filteredProviderModels.map((m) => (
                        <label key={m.id} className="flex items-center gap-2 px-3 py-1.5 text-sm hover:bg-accent cursor-pointer">
                          <Checkbox
                            checked={selectedPMs.has(m.id)}
                            onCheckedChange={() => togglePM(m.id)}
                          />
                          <span className="truncate font-mono text-xs">{m.id}</span>
                          {m.owned_by && <span className="ml-auto text-[10px] text-muted-foreground shrink-0">{m.owned_by}</span>}
                        </label>
                      ))}
                      {filteredProviderModels.length === 0 && providerModels.length > 0 && (
                        <div className="px-3 py-2 text-xs text-muted-foreground">{t("batch_association.no_pm_match")}</div>
                      )}
                    </div>
                  )}
                  {/* 手动添加：拉不到列表（#54）或名字不在列表里时的入口 */}
                  <div className="flex gap-2">
                    <Input
                      className="h-8 text-xs font-mono"
                      placeholder={t("batch_association.manual_add_placeholder")}
                      value={manualName}
                      onChange={(e) => setManualName(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") {
                          e.preventDefault();
                          const name = manualName.trim();
                          if (name) { togglePM(name); setManualName(""); }
                        }
                      }}
                    />
                    <Button type="button" variant="outline" size="sm" className="h-8 shrink-0"
                      onClick={() => {
                        const name = manualName.trim();
                        if (name) { togglePM(name); setManualName(""); }
                      }}>
                      {t("batch_association.manual_add")}
                    </Button>
                  </div>
                </>
              )}

              {manualPMs.length > 0 && (
                <div className="flex flex-wrap gap-1">
                  {manualPMs.map((pm) => (
                    <span key={pm} className="inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-mono">
                      {pm}
                      <button type="button" aria-label={t("batch_association.remove_manual", { name: pm })} onClick={() => togglePM(pm)}>
                        <X className="size-3" />
                      </button>
                    </span>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* 逻辑模型 */}
          {mode === "same-name" ? (
            providerId > 0 && selectedPMs.size > 0 && (
              <div className="rounded-md border max-h-40 overflow-y-auto">
                {sameNamePairs.map(({ pm, model }) => (
                  <div key={pm} className="flex items-center justify-between px-3 py-1.5 text-xs">
                    <span className="font-mono truncate">{pm}</span>
                    {model ? (
                      <span className="text-muted-foreground shrink-0 ml-2">
                        {t("batch_association.same_name_target", { id: model.ID })}
                      </span>
                    ) : (
                      <span className="text-status-critical-ink shrink-0 ml-2">
                        {t("batch_association.no_same_name")}
                      </span>
                    )}
                  </div>
                ))}
              </div>
            )
          ) : (
            <div className="space-y-2">
              <Label className="text-xs">
                {t("batch_association.models")}
                <span className="ml-2 text-muted-foreground">
                  {t("batch_association.selected_count", { count: selectedModelIds.size })}
                </span>
              </Label>
              {lockedModelId ? (
                <div className="rounded-md border px-3 py-2 text-sm">
                  {modelById.get(lockedModelId)?.Name ?? lockedModelId}
                </div>
              ) : (
                <>
                  <div className="relative">
                    <Search className="size-3.5 absolute left-2 top-1/2 -translate-y-1/2 text-muted-foreground" />
                    <Input
                      className="h-8 pl-7 text-xs"
                      placeholder={t("batch_association.model_search_placeholder")}
                      value={modelSearch}
                      onChange={(e) => setModelSearch(e.target.value)}
                    />
                  </div>
                  <div className="max-h-40 overflow-y-auto rounded-md border">
                    {filteredModels.map((m) => (
                      <label key={m.ID} className="flex items-center gap-2 px-3 py-1.5 text-sm hover:bg-accent cursor-pointer">
                        <Checkbox
                          checked={selectedModelIds.has(m.ID)}
                          onCheckedChange={() => {
                            setSelectedModelIds((prev) => {
                              const next = new Set(prev);
                              if (next.has(m.ID)) next.delete(m.ID);
                              else next.add(m.ID);
                              return next;
                            });
                          }}
                        />
                        <span className="truncate">{m.Name}</span>
                      </label>
                    ))}
                  </div>
                </>
              )}
            </div>
          )}

          {/* 公共配置 */}
          <div className="space-y-3 rounded-md border p-3">
            <p className="text-sm font-medium">{t("batch_association.defaults")}</p>
            <div className="grid grid-cols-2 gap-3">
              <div className="flex flex-col gap-1 text-xs">
                <Label>{t("association_form.weight")}</Label>
                <Input
                  type="number" min="1" className="h-8 text-xs"
                  aria-label={t("association_form.weight")}
                  value={defaults.weight}
                  onChange={(e) => setDefaults((d) => ({ ...d, weight: parseInt(e.target.value) || 0 }))}
                />
              </div>
              <div className="flex flex-col gap-1 text-xs">
                <Label>{t("batch_association.currency")}</Label>
                <Select value={defaults.currency} onValueChange={(v) => setDefaults((d) => ({ ...d, currency: v }))}>
                  <SelectTrigger className="h-8 text-xs px-2"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="auto">{t("batch_association.currency_auto")}</SelectItem>
                    <SelectItem value="CNY">CNY (¥)</SelectItem>
                    <SelectItem value="USD">USD ($)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="grid grid-cols-3 gap-3">
              {(["tool_call", "structured_output", "image"] as const).map((cap) => (
                <div key={cap} className="flex flex-col gap-1 text-xs">
                  <Label>{t(`association_form.${cap === "image" ? "vision" : cap}`)}</Label>
                  <Select
                    value={defaults[cap]}
                    onValueChange={(v) => setDefaults((d) => ({ ...d, [cap]: v as CapabilityTriState }))}
                  >
                    <SelectTrigger className="h-8 text-xs px-2"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="auto">{t("batch_association.cap_auto")}</SelectItem>
                      <SelectItem value="on">{t("batch_association.cap_on")}</SelectItem>
                      <SelectItem value="off">{t("batch_association.cap_off")}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              ))}
            </div>
            <div className="grid grid-cols-3 gap-3">
              {(["input_price", "cache_read_price", "output_price"] as const).map((price) => (
                <div key={price} className="flex flex-col gap-1 text-xs">
                  <Label>{t(`association_form.${price}`)}</Label>
                  <Input
                    type="number" step="0.25" min="0" className="h-8 text-xs font-mono"
                    placeholder={t("batch_association.price_auto")}
                    value={defaults[price]}
                    onChange={(e) => setDefaults((d) => ({ ...d, [price]: e.target.value }))}
                  />
                </div>
              ))}
            </div>
            <div className="flex flex-wrap gap-4">
              <label className="flex items-center gap-2 text-xs">
                <Checkbox
                  checked={defaults.with_header}
                  onCheckedChange={(v) => setDefaults((d) => ({ ...d, with_header: v === true }))}
                />
                {t("association_form.with_header")}
              </label>
              <label className="flex items-center gap-2 text-xs">
                <Checkbox
                  checked={defaults.backup}
                  onCheckedChange={(v) => setDefaults((d) => ({ ...d, backup: v === true }))}
                />
                {t("association_form.backup")}
              </label>
            </div>
            <p className="text-[11px] text-muted-foreground">{t("batch_association.defaults_hint")}</p>
            <p className="text-[11px] text-muted-foreground">{t("batch_association.quota_hint")}</p>
            {priceInvalid && (
              <p className="text-[11px] text-status-critical-ink">{t("batch_association.price_invalid")}</p>
            )}
            {defaults.backup && (
              <p className="text-[11px] text-status-critical-ink">{t("batch_association.all_backup_warning")}</p>
            )}
          </div>

          {/* 预览 */}
          {preview && (
            <div className="space-y-2 rounded-md border bg-muted/30 p-3">
              <p className="text-sm font-medium">{t("batch_association.preview_title")}</p>
              <p className="text-xs">
                {t("batch_association.preview_counts", {
                  create: preview.created.length,
                  skip: preview.skipped.length,
                })}
              </p>
              {preview.created
                .filter((c) => modelById.get(c.ModelID)?.Name !== c.ProviderModel)
                .length > 0 && (
                <p className="text-[11px] text-status-critical-ink">
                  {t("batch_association.non_same_name_note")}
                </p>
              )}
              {preview.skipped.length > 0 && (
                <div className="max-h-28 overflow-y-auto text-xs">
                  {preview.skipped.map((s, i) => (
                    <div key={i} className="flex items-center gap-2">
                      <span className="font-mono">{s.provider_model}</span>
                      <span className="text-muted-foreground">
                        {modelById.get(s.model_id)?.Name ?? s.model_id}
                      </span>
                      <span className="ml-auto text-muted-foreground">
                        {s.existing_status === false ? t("batch_association.skipped_disabled") : t("batch_association.skipped_exists")}
                      </span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* 结果 */}
          {result && (
            <div className="space-y-2 rounded-md border p-3">
              <p className="text-sm font-medium">{t("batch_association.result_title")}</p>
              <p className="text-xs">
                {t("batch_association.result_counts", {
                  create: result.created.length,
                  skip: result.skipped.length,
                  fail: result.failed.length,
                })}
              </p>
              {result.created.length > 0 && (
                <div className="max-h-44 overflow-y-auto text-xs divide-y">
                  {result.created.map((c) => (
                    <div key={`${c.ModelID}-${c.ProviderModel}`} className="py-1.5">
                      <div className="flex items-center gap-2">
                        <span className="font-mono truncate">{c.ProviderModel}</span>
                        <span className="text-muted-foreground">
                          {modelById.get(c.ModelID)?.Name ?? c.ModelID}
                        </span>
                        {modelById.get(c.ModelID)?.Name !== c.ProviderModel && (
                          <span className="text-[10px] rounded-full border px-1.5">{t("batch_association.non_same_name_badge")}</span>
                        )}
                      </div>
                      {c.Autofill?.filled && Object.keys(c.Autofill.filled).length > 0 && (
                        <p className="text-[11px] text-muted-foreground mt-0.5">
                          {t("batch_association.autofill_summary", {
                            source: c.Autofill.source ?? "",
                            fields: Object.entries(c.Autofill.filled)
                              .map(([k, v]) => `${k}=${String(v)}`)
                              .join(", "),
                          })}
                        </p>
                      )}
                    </div>
                  ))}
                </div>
              )}
              {result.skipped.length > 0 && (
                <div className="max-h-24 overflow-y-auto text-xs">
                  {result.skipped.map((s, i) => (
                    <div key={i} className="flex items-center gap-2">
                      <span className="font-mono">{s.provider_model}</span>
                      <span className="text-muted-foreground">
                        {modelById.get(s.model_id)?.Name ?? s.model_id}
                      </span>
                      <span className="ml-auto text-muted-foreground">
                        {s.existing_status === false ? t("batch_association.skipped_disabled") : t("batch_association.skipped_exists")}
                      </span>
                    </div>
                  ))}
                </div>
              )}
              {result.unfilled.length > 0 && (
                <div className="text-[11px] text-muted-foreground">
                  {t("batch_association.unfilled_header")}
                  {result.unfilled.map((u, i) => (
                    <span key={i} className="ml-1 font-mono">
                      {u.provider_model}({reasonText(u.reason)})
                    </span>
                  ))}
                </div>
              )}
              {result.failed.length > 0 && (
                <div className="max-h-28 overflow-y-auto text-xs">
                  {result.failed.map((f, i) => (
                    <div key={i} className="flex items-center gap-2">
                      <span className="font-mono">{f.provider_model}</span>
                      <span className="text-muted-foreground">{modelById.get(f.model_id)?.Name ?? f.model_id}</span>
                      <span className="ml-auto text-status-critical-ink">{reasonText(f.reason)}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </div>

        <DialogFooter className="gap-2">
          <Button type="button" variant="outline" onClick={handleClose}>
            {result ? t("common:actions.close") : t("common:actions.cancel")}
          </Button>
          {!result && (
            <>
              {!preview ? (
                <Button type="button" disabled={!canPreview || previewing} onClick={handlePreview}>
                  {previewing ? <Spinner className="size-4" /> : null}
                  {t("batch_association.preview")}
                </Button>
              ) : (
                <Button type="button" disabled={submitting || preview.created.length === 0} onClick={handleConfirm}>
                  {submitting ? <Spinner className="size-4" /> : null}
                  {t("batch_association.confirm_create", { count: preview.created.length })}
                </Button>
              )}
            </>
          )}
          {result && result.failed.length > 0 && (
            <Button type="button" variant="outline" disabled={submitting} onClick={handleRetryFailed}>
              {t("batch_association.retry_failed", { count: result.failed.length })}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
