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
import { Search } from "lucide-react";
import {
  batchCreateModelProviders,
  batchCreateModels,
  getModelMetadata,
  type BatchCreateResult,
  type BatchModelsResult,
} from "@/lib/api";
import type { Model, ModelWithProvider, Provider, ProviderModel } from "@/lib/api";
import { getModelProviders } from "@/lib/api";

/**
 * 「从供应商导入」向导（方案 §4.2.2）。
 *
 * 两步编排而非一个"batch-import"端点：第一步批量建逻辑模型（幂等，同名
 * 跳过），第二步用 pairs 逐对挂关联（不产生笛卡尔交叉）。每一步都可幂等
 * 重试，失败报告带"重试关联"出口。
 *
 * 差异确认页把三件事说在下单前：将要新建多少个逻辑模型（它们立即出现在
 * 客户端 /v1/models）、哪些会挂到已有同名模型、元数据源是否就绪。
 */

type Step = 1 | 2 | 3;

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  models: Model[];
  providers: Provider[];
  providerModelsMap: Record<number, ProviderModel[]>;
  providerModelsLoading: Record<number, boolean>;
  loadProviderModels: (providerId: number, force?: boolean) => Promise<void>;
  onCompleted: () => void | Promise<void>;
};

/** 向导里的模型默认值：本地全是非空值（输入框不吃 null），发送时按需组包 */
type WizardModelDefaults = { strategy: string; max_retry: number; time_out: number };

const emptyModelDefaults: WizardModelDefaults = {
  strategy: "lottery",
  max_retry: 3,
  time_out: 300,
};

export function ImportWizard({
  open,
  onOpenChange,
  models,
  providers,
  providerModelsMap,
  providerModelsLoading,
  loadProviderModels,
  onCompleted,
}: Props) {
  const { t } = useTranslation(["models", "common"]);
  const [step, setStep] = useState<Step>(1);
  const [providerId, setProviderId] = useState(0);
  const [pmSearch, setPmSearch] = useState("");
  const [selectedPMs, setSelectedPMs] = useState<Set<string>>(new Set());
  const [pasteMode, setPasteMode] = useState(false);
  const [pasteText, setPasteText] = useState("");
  const [attachExisting, setAttachExisting] = useState(true);
  const [modelDefaults, setModelDefaults] = useState(emptyModelDefaults);
  const [catalogWarning, setCatalogWarning] = useState(false);
  const [probingCatalog, setProbingCatalog] = useState(false);
  const [probingConn, setProbingConn] = useState(false);
  const [executing, setExecuting] = useState(false);
  const [stepAResult, setStepAResult] = useState<BatchModelsResult | null>(null);
  const [stepBResult, setStepBResult] = useState<BatchCreateResult | null>(null);
  /** 提交成功后已即时通知页面刷新（notifyRefresh 置位）；关闭时跳过兜底补拉 */
  const refreshedRef = useRef(false);

  useEffect(() => {
    if (!open) return;
    setStep(1);
    setProviderId(0);
    setSelectedPMs(new Set());
    setPmSearch("");
    setPasteMode(false);
    setPasteText("");
    setAttachExisting(true);
    setModelDefaults(emptyModelDefaults);
    setCatalogWarning(false);
    setStepAResult(null);
    setStepBResult(null);
    refreshedRef.current = false;
  }, [open]);

  useEffect(() => {
    if (open && providerId > 0) {
      void loadProviderModels(providerId);
    }
  }, [open, providerId, loadProviderModels]);

  const providerModels = useMemo(
    () => (providerId ? providerModelsMap[providerId] || [] : []),
    [providerId, providerModelsMap],
  );
  const modelByName = useMemo(() => new Map(models.map((m) => [m.Name, m])), [models]);

  const filteredProviderModels = useMemo(() => {
    const q = pmSearch.trim().toLowerCase();
    if (!q) return providerModels;
    return providerModels.filter((m) => m.id.toLowerCase().includes(q));
  }, [providerModels, pmSearch]);

  const classification = useMemo(() => {
    const toCreate: string[] = [];
    const toAttach: string[] = [];
    for (const pm of selectedPMs) {
      if (modelByName.has(pm)) {
        toAttach.push(pm);
      } else {
        toCreate.push(pm);
      }
    }
    return { toCreate, toAttach };
  }, [selectedPMs, modelByName]);

  // attach 的那些模型里，哪些已经有同一条关联（提交时会被 skip）——
  // 差异页不猜，直接跑一次 dry_run 拿服务端的判定。
  const [attachPreview, setAttachPreview] = useState<BatchCreateResult | null>(null);
  useEffect(() => {
    if (step !== 2 || !attachExisting || classification.toAttach.length === 0 || !providerId) {
      setAttachPreview(null);
      return
    }
    let cancelled = false;
    batchCreateModelProviders({
      model_ids: [],
      provider_id: providerId,
      provider_models: [],
      pairs: classification.toAttach.map((pm) => ({ model_id: modelByName.get(pm)!.ID, provider_model: pm })),
      defaults: { weight: 1, with_header: false, backup: false },
      autofill_meta: false,
      dry_run: true,
    }).then((res) => {
      if (!cancelled) setAttachPreview(res);
    }).catch(() => {
      if (!cancelled) setAttachPreview(null);
    });
    return () => {
      cancelled = true;
    };
  }, [step, attachExisting, classification.toAttach, modelByName, providerId]);

  // 熔断建议（方案 §4.2.2-5）：批量把主关联数推到 MaxRetry 之上后，backup 的
  // 接管前提（请求内主池清空）会被静默破坏——除非模型开了熔断（熔断 Open 的
  // 关联立即出池，主池才可能被清空）。挂到已有模型前检查一遍，把这笔账说在
  // 下单前；不改默认值，只给建议与解释。
  const [breakerAdvice, setBreakerAdvice] = useState<string[] | null>(null);
  useEffect(() => {
    if (step !== 2 || !attachExisting || classification.toAttach.length === 0) {
      setBreakerAdvice(null);
      return;
    }
    let cancelled = false;
    Promise.all(
      classification.toAttach
        .map((pm) => modelByName.get(pm))
        .filter((m): m is Model => !!m)
        .map(async (m) => {
          const assocs = await getModelProviders(m.ID).catch(() => [] as ModelWithProvider[]);
          const primary = assocs.filter((a) => !a.Backup && (a.Status ?? false));
          // 本次导入将新增的主关联也算进去：挂完之后才是真实的池规模
          const willAdd = attachPreview
            ? attachPreview.created.filter((c) => c.ModelID === m.ID && !c.Backup).length
            : 0;
          const total = primary.length + willAdd;
          return total > m.MaxRetry && !m.Breaker
            ? t("import_wizard.breaker_advice_item", { name: m.Name, count: total, maxRetry: m.MaxRetry })
            : null;
        }),
    ).then((items) => {
      if (!cancelled) setBreakerAdvice(items.filter((x): x is string => !!x));
    });
    return () => {
      cancelled = true;
    };
  }, [step, attachExisting, classification.toAttach, modelByName, attachPreview, t]);

  /** 元数据源就绪探测：差异页进来看一眼，未就绪就把话说在导入之前 */
  useEffect(() => {
    if (step !== 2 || !providerId) return;
    const probe = [...selectedPMs][0];
    if (!probe) return;
    let cancelled = false;
    setProbingCatalog(true);
    getModelMetadata(providerId, probe)
      .then((sug) => {
        if (!cancelled) {
          setCatalogWarning(!sug.matched && sug.reason === "catalog_unavailable");
        }
      })
      .catch(() => {
        if (!cancelled) setCatalogWarning(false);
      })
      .finally(() => {
        if (!cancelled) setProbingCatalog(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, providerId]);

  const togglePM = (pm: string) => {
    setSelectedPMs((prev) => {
      const next = new Set(prev);
      if (next.has(pm)) next.delete(pm);
      else next.add(pm);
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

  const allVisibleSelected =
    filteredProviderModels.length > 0 && filteredProviderModels.every((m) => selectedPMs.has(m.id));

  /** 连通性探活：按供应商去重，拉一次模型列表就是一发真实请求 */
  const probeConnectivity = async () => {
    if (!providerId) return;
    setProbingConn(true);
    try {
      await loadProviderModels(providerId, true);
      toast.success(t("import_wizard.probe_ok"));
    } catch (err) {
      toast.error(t("import_wizard.probe_failed", { message: err instanceof Error ? err.message : String(err) }));
    } finally {
      setProbingConn(false);
    }
  };

  const runImport = async () => {
    if (!providerId) return;
    setExecuting(true);
    try {
      // 第一步：建缺的逻辑模型（幂等）
      let aResult: BatchModelsResult = { created: [], skipped: [] };
      if (classification.toCreate.length > 0) {
        aResult = await batchCreateModels(classification.toCreate, {
          strategy: modelDefaults.strategy,
          max_retry: modelDefaults.max_retry,
          time_out: modelDefaults.time_out,
        });
      }
      setStepAResult(aResult);

      // 第二步：pairs 逐对挂关联（新建 + attach 到已有同名）
      const createdIdByName = new Map(aResult.created.map((m) => [m.Name, m.ID]));
      const pairs: { model_id: number; provider_model: string }[] = [];
      for (const pm of classification.toCreate) {
        const id = createdIdByName.get(pm);
        if (id) pairs.push({ model_id: id, provider_model: pm });
      }
      if (attachExisting) {
        for (const pm of classification.toAttach) {
          const id = modelByName.get(pm)?.ID;
          if (id) pairs.push({ model_id: id, provider_model: pm });
        }
      }
      let bResult: BatchCreateResult = { dry_run: false, created: [], skipped: [], failed: [], unfilled: [] };
      if (pairs.length > 0) {
        bResult = await batchCreateModelProviders({
          model_ids: [],
          provider_id: providerId,
          provider_models: [],
          pairs,
          defaults: { weight: 1, with_header: false, backup: false },
        });
      }
      setStepBResult(bResult);
      setStep(3);
      // 两步都已落库就立刻刷新页面数据：新建的逻辑模型与关联不该等关掉
      // 向导才出现在总览里。onCompleted 无参（页面整表重拉）。
      notifyRefresh();
    } catch (err) {
      toast.error(t("import_wizard.exec_failed", { message: err instanceof Error ? err.message : String(err) }));
    } finally {
      setExecuting(false);
    }
  };

  /** 提交成功后的即时页面刷新。幂等：重试成功后再调一次，代价是重复拉取 */
  const notifyRefresh = () => {
    refreshedRef.current = true;
    void onCompleted();
  };

  const retryAssociations = async () => {
    if (!stepBResult || !providerId) return;
    setExecuting(true);
    try {
      // 名字 → id 的映射必须把第一步新建的模型也算进来：它们还不在 models
      // prop 里（页面要等弹窗关了才重拉），只查 modelByName 会全部落空，
      // 重试就成了无声的空操作。
      const idByName = new Map<string, number>([
        ...[...modelByName].map(([name, m]) => [name, m.ID] as const),
        ...stepAResult?.created.map((m) => [m.Name, m.ID] as const) ?? [],
      ]);
      const pairs: { model_id: number; provider_model: string }[] = [];
      for (const f of stepBResult.failed) {
        const id = idByName.get(f.provider_model);
        if (id && id === f.model_id) pairs.push({ model_id: id, provider_model: f.provider_model });
      }
      if (pairs.length === 0) return;
      const retried = await batchCreateModelProviders({
        model_ids: [], provider_id: providerId, provider_models: [],
        pairs, defaults: { weight: 1, with_header: false, backup: false },
      });
      setStepBResult({
        dry_run: false,
        created: [...stepBResult.created, ...retried.created],
        skipped: [...stepBResult.skipped, ...retried.skipped],
        failed: retried.failed,
        unfilled: [
          ...new Map(
            [...stepBResult.unfilled, ...retried.unfilled].map((u) => [`${u.provider_model}\u0000${u.reason}`, u] as const),
          ).values(),
        ],
      });
      notifyRefresh();
    } catch (err) {
      toast.error(t("import_wizard.exec_failed", { message: err instanceof Error ? err.message : String(err) }));
    } finally {
      setExecuting(false);
    }
  };

  const handleClose = async () => {
    // 提交成功时已即时刷新（notifyRefresh）；这里只在"有改动但还没通知过"
    // 的兜底场景（如提交后的失败重试全被取消）补拉一次
    if (!refreshedRef.current) {
      const touched =
        (stepAResult?.created.length ?? 0) > 0 ||
        (stepBResult?.created.length ?? 0) > 0 ||
        (stepBResult?.skipped.length ?? 0) > 0;
      if (touched) {
        await onCompleted();
      }
    }
    onOpenChange(false);
  };

  const attachExistingSkip = attachPreview
    ? new Set(attachPreview.skipped.map((s) => s.provider_model))
    : new Set<string>();

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) void handleClose(); }}>
      <DialogContent className="max-h-[85vh] flex flex-col" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{t("import_wizard.title")}</DialogTitle>
          <DialogDescription>{t("import_wizard.desc")}</DialogDescription>
        </DialogHeader>

        <div className="space-y-4 overflow-y-auto pr-1 flex-1 min-h-0">
          {step === 1 && (
            <>
              <div className="flex flex-col gap-1 text-xs">
                <Label>{t("batch_association.provider")}</Label>
                <Select value={providerId ? providerId.toString() : ""} onValueChange={(v) => { setProviderId(Number(v)); setSelectedPMs(new Set()); }}>
                  <SelectTrigger className="h-8 text-xs px-2">
                    <SelectValue placeholder={t("batch_association.provider_placeholder")} />
                  </SelectTrigger>
                  <SelectContent>
                    {providers.map((p) => (
                      <SelectItem key={p.ID} value={p.ID.toString()}>{p.Name}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {providerId > 0 && (
                  <div className="flex items-center gap-2 mt-1">
                    <Button type="button" variant="ghost" size="sm" className="h-7 text-xs" onClick={probeConnectivity} disabled={probingConn}>
                      {probingConn ? <Spinner className="size-3.5" /> : null}
                      {t("import_wizard.probe")}
                    </Button>
                    <span className="text-muted-foreground">{t("import_wizard.probe_hint")}</span>
                  </div>
                )}
              </div>

              {providerId > 0 && (
                <div className="space-y-2">
                  <div className="flex items-center justify-between">
                    <Label className="text-xs">
                      {t("batch_association.provider_models")}
                      <span className="ml-2 text-muted-foreground">
                        {t("batch_association.selected_count", { count: selectedPMs.size })}
                      </span>
                    </Label>
                    {!pasteMode ? (
                      <div className="flex items-center gap-1">
                        <Button type="button" variant="ghost" size="sm" onClick={() => {
                          setSelectedPMs((prev) => {
                            const next = new Set(prev);
                            if (allVisibleSelected) {
                              for (const m of filteredProviderModels) next.delete(m.id);
                            } else {
                              for (const m of filteredProviderModels) next.add(m.id);
                            }
                            return next;
                          });
                        }} disabled={providerModels.length === 0}>
                          {allVisibleSelected ? t("batch_association.unselect_all") : t("batch_association.select_all")}
                        </Button>
                        <Button type="button" variant="ghost" size="sm" onClick={() => setPasteMode(true)}>
                          {t("batch_association.paste_mode")}
                        </Button>
                      </div>
                    ) : (
                      <Button type="button" variant="ghost" size="sm" onClick={() => setPasteMode(false)}>
                        {t("batch_association.list_mode")}
                      </Button>
                    )}
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
                      ) : (
                        <div className="max-h-56 overflow-y-auto rounded-md border">
                          {filteredProviderModels.map((m) => (
                            <label key={m.id} className="flex items-center gap-2 px-3 py-1.5 text-sm hover:bg-accent cursor-pointer">
                              <Checkbox checked={selectedPMs.has(m.id)} onCheckedChange={() => togglePM(m.id)} />
                              <span className="truncate font-mono text-xs">{m.id}</span>
                              {modelByName.has(m.id) && (
                                <span className="ml-auto text-[10px] text-muted-foreground shrink-0">
                                  {t("import_wizard.same_name_exists")}
                                </span>
                              )}
                            </label>
                          ))}
                          {filteredProviderModels.length === 0 && (
                            <div className="px-3 py-2 text-xs text-muted-foreground">
                              {providerModels.length === 0
                                ? t("batch_association.no_models_fetched")
                                : t("batch_association.no_pm_match")}
                            </div>
                          )}
                        </div>
                      )}
                    </>
                  )}
                </div>
              )}
            </>
          )}

          {step === 2 && (
            <>
              {/* 差异确认页：三件事说在下单前 */}
              <div className="rounded-md border bg-muted/30 p-3 space-y-1">
                <p className="text-sm font-medium">{t("import_wizard.diff_title")}</p>
                <p className="text-xs">
                  {t("import_wizard.diff_create", { count: classification.toCreate.length })}
                </p>
                <p className="text-xs">
                  {/* 计数随 attachExisting 收缩：摘要说的是"这次将发生什么"，
                      取消挂载后还按候选全集报数就与下方清单自相矛盾 */}
                  {t("import_wizard.diff_attach", {
                    count: attachExisting ? classification.toAttach.length : 0,
                  })}
                </p>
                {classification.toCreate.length > 0 && (
                  <p className="text-[11px] text-status-critical-ink">
                    {t("import_wizard.models_visible_warning", { count: classification.toCreate.length })}
                  </p>
                )}
                {catalogWarning && (
                  <p className="text-[11px] text-status-critical-ink">
                    {t("import_wizard.catalog_warning")}
                  </p>
                )}
                {probingCatalog && <p className="text-[11px] text-muted-foreground">{t("import_wizard.catalog_probing")}</p>}
                {breakerAdvice && breakerAdvice.length > 0 && (
                  <div className="text-[11px] text-status-critical-ink space-y-0.5">
                    <p>{t("import_wizard.breaker_advice_title")}</p>
                    {breakerAdvice.map((line) => (
                      <p key={line} className="pl-2 font-mono">{line}</p>
                    ))}
                    <p className="text-muted-foreground">{t("import_wizard.breaker_advice_hint")}</p>
                  </div>
                )}
              </div>

              <div className="max-h-40 overflow-y-auto rounded-md border">
                {[...classification.toCreate, ...(attachExisting ? classification.toAttach : [])]
                  .sort()
                  .map((pm) => (
                    <div key={pm} className="flex items-center justify-between px-3 py-1.5 text-xs">
                      <span className="font-mono truncate">{pm}</span>
                      <span className="text-muted-foreground shrink-0 ml-2">
                        {classification.toCreate.includes(pm)
                          ? t("import_wizard.will_create_model")
                          : attachExistingSkip.has(pm)
                            ? t("import_wizard.existing_assoc")
                            : t("import_wizard.will_attach")}
                      </span>
                    </div>
                  ))}
                {/* 渲染条件不带 attachExisting：勾掉后这个 checkbox 就是唯一
                    的再勾回入口，条件里带上它就成了单向门 */}
                {classification.toAttach.length > 0 && (
                  <label className="flex items-center gap-2 px-3 py-2 text-xs border-t">
                    <Checkbox
                      checked={attachExisting}
                      onCheckedChange={(v) => setAttachExisting(v === true)}
                    />
                    {t("import_wizard.attach_policy")}
                  </label>
                )}
              </div>

              {/* 新建逻辑模型的公共默认 */}
              <div className="grid grid-cols-4 gap-3 rounded-md border p-3">
                <div className="flex flex-col gap-1 text-xs col-span-1">
                  <Label>{t("import_wizard.model_strategy")}</Label>
                  <Select value={modelDefaults.strategy} onValueChange={(v) => setModelDefaults((d) => ({ ...d, strategy: v }))}>
                    <SelectTrigger className="h-8 text-xs px-2"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="lottery">Lottery</SelectItem>
                      <SelectItem value="rotor">Rotor</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="flex flex-col gap-1 text-xs">
                  <Label>{t("import_wizard.model_max_retry")}</Label>
                  <Input type="number" min="1" className="h-8 text-xs" value={modelDefaults.max_retry}
                    onChange={(e) => setModelDefaults((d) => ({ ...d, max_retry: parseInt(e.target.value) || 0 }))} />
                </div>
                <div className="flex flex-col gap-1 text-xs">
                  <Label>{t("import_wizard.model_timeout")}</Label>
                  <Input type="number" min="1" className="h-8 text-xs" value={modelDefaults.time_out}
                    onChange={(e) => setModelDefaults((d) => ({ ...d, time_out: parseInt(e.target.value) || 0 }))} />
                </div>
              </div>
              <p className="text-[11px] text-muted-foreground">{t("import_wizard.defaults_hint")}</p>
            </>
          )}

          {step === 3 && stepBResult && (
            <div className="space-y-2 rounded-md border p-3">
              <p className="text-sm font-medium">{t("import_wizard.result_title")}</p>
              <p className="text-xs">
                {t("batch_association.result_counts", {
                  create: stepBResult.created.length,
                  skip: stepBResult.skipped.length,
                  fail: stepBResult.failed.length,
                })}
              </p>
              {stepAResult && stepAResult.created.length > 0 && (
                <p className="text-[11px] text-muted-foreground">
                  {t("import_wizard.models_created", { count: stepAResult.created.length })}
                </p>
              )}
              {stepBResult.failed.length > 0 && (
                <div className="max-h-28 overflow-y-auto text-xs">
                  {stepBResult.failed.map((f, i) => (
                    <div key={i} className="flex items-center gap-2">
                      <span className="font-mono">{f.provider_model}</span>
                      <span className="ml-auto text-status-critical-ink">{f.reason}</span>
                    </div>
                  ))}
                </div>
              )}
              {stepBResult.unfilled.length > 0 && (
                <p className="text-[11px] text-muted-foreground">
                  {t("batch_association.unfilled_header")}
                  {stepBResult.unfilled.map((u) => u.provider_model).join(", ")}
                  {t("import_wizard.backfill_hint")}
                </p>
              )}
            </div>
          )}
        </div>

        <DialogFooter className="gap-2">
          <Button type="button" variant="outline" onClick={() => void handleClose()}>
            {step === 3 ? t("common:actions.close") : t("common:actions.cancel")}
          </Button>
          {step === 2 && (
            <Button type="button" variant="outline" onClick={() => setStep(1)}>
              {t("import_wizard.back")}
            </Button>
          )}
          {step === 1 && (
            <Button
              type="button"
              disabled={providerId === 0 || selectedPMs.size === 0}
              onClick={() => setStep(2)}
            >
              {t("import_wizard.next")}
            </Button>
          )}
          {step === 2 && (
            <Button
              type="button"
              disabled={
                executing ||
                // 后端会把 <1 的数值整批打回（0 是"必超时/零重试"毒药），
                // 这里先按住按钮，别让一次误击吞掉整批
                modelDefaults.max_retry < 1 ||
                modelDefaults.time_out < 1
              }
              onClick={() => void runImport()}
            >
              {executing ? <Spinner className="size-4" /> : null}
              {t("import_wizard.import", { count: selectedPMs.size })}
            </Button>
          )}
          {step === 3 && stepBResult !== null && stepBResult.failed.length > 0 && (
            <Button type="button" variant="outline" disabled={executing} onClick={() => void retryAssociations()}>
              {t("batch_association.retry_failed", { count: stepBResult.failed.length })}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
