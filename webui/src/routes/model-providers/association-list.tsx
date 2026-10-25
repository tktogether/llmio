import { Pencil, RefreshCw, Trash2, Zap } from "lucide-react"
import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"

import Loading from "@/components/loading"
import { ErrorState } from "@/components/state-views"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Spinner } from "@/components/ui/spinner"
import type { ModelWithProvider, Provider } from "@/lib/api"
import { MobileInfoItem } from "@/components/mobile-info-item"

/**
 * 能力列的对勾/叉号。
 *
 * 两个符号本身没有可读的名字（读屏会念成"勾""叉"甚至跳过），因此带一个
 * 已翻译的 aria-label；颜色只在语义色里取，不再用 green-600/red-600 这套
 * 固定色——深色模式下那两个色号与背景的对比度不够。
 */
const Mark = ({ ok, label }: { ok: boolean; label: string }) => (
  <span
    role="img"
    aria-label={label}
    className={ok ? "text-status-good-ink" : "text-status-critical-ink"}
  >
    {ok ? "✓" : "✗"}
  </span>
)

type Props = {
  /** 已筛选、已排序的关联；筛选与排序都在页面里做完 */
  associations: ModelWithProvider[]
  providers: Provider[]
  /** 每个关联的近期成败条：缺键即"还没取到"，取到空数组即"没有数据"——两者说法不同 */
  providerStatus: Record<number, boolean[]>
  statusUpdating: Record<number, boolean>
  /** 正在确认删除的那条关联 ID */
  deleteId: number | null
  loading: boolean
  /** 取数失败的原文：有值就先说失败，绝不说"这个模型没有关联" */
  error: string | null
  onRetry: () => void
  emptyText: string
  onRefreshStatus: () => void
  onEdit: (association: ModelWithProvider) => void
  onTest: (associationId: number) => void
  onOpenDelete: (associationId: number) => void
  onCloseDelete: () => void
  onConfirmDelete: () => void
  onStatusToggle: (association: ModelWithProvider, nextStatus: boolean) => void
  // ── 批量运维（仅桌面表格；手机卡片首版不支持多选，成对实现的约定在此豁免）──
  /** 当前选中的关联 ID；由页面持有，筛选后隐藏的行会被页面清出选区 */
  selectedIds: number[]
  onToggleSelect: (id: number) => void
  onToggleSelectAll: () => void
  /** 任一批量请求在途时禁用全部批量按钮 */
  bulkPending: boolean
  onBulkStatus: (status: boolean) => void
  onBulkDelete: (ids: number[]) => void
  onBulkTest: (ids: number[]) => void
  /** 补全空位元数据（价格为 0 的档位会被数据源价覆盖，确认对话框已声明） */
  onBackfill: (ids: number[]) => void
  /** 批量测试进度：null = 不在跑 */
  bulkTestProgress: { done: number; total: number } | null
  /** weight 行内快编：页面负责 PUT 全量 payload 并回填列表 */
  onWeightSave: (association: ModelWithProvider, weight: number) => Promise<void>
}

/**
 * 某个模型下的关联列表（面板整体：加载 / 空 / 有数据）。
 *
 * 与模型列表同样是桌面表格 + 手机卡片两套并行实现，改动要成对改。
 * 状态条有两种"没有"：`providerStatus` 里没有这个键 = 还在取，取到空数组 =
 * 取到了但近期没有记录，所以一处转圈、一处写"无数据"。
 */
export function AssociationList({
  associations,
  providers,
  providerStatus,
  statusUpdating,
  deleteId,
  loading,
  error,
  onRetry,
  emptyText,
  onRefreshStatus,
  onEdit,
  onTest,
  onOpenDelete,
  onCloseDelete,
  onConfirmDelete,
  onStatusToggle,
  selectedIds,
  onToggleSelect,
  onToggleSelectAll,
  bulkPending,
  onBulkStatus,
  onBulkDelete,
  onBulkTest,
  onBackfill,
  bulkTestProgress,
  onWeightSave,
}: Props) {
  const { t } = useTranslation(["models", "common"])

  const selectedIdSet = new Set(selectedIds)
  const allVisibleSelected =
    associations.length > 0 && associations.every((a) => selectedIdSet.has(a.ID))

  /** 批量删除确认：N≥5 时确认按钮延迟 2 秒激活——误触全选后的一次点按
   *  不该能清空一个模型的全部关联。 */
  const [bulkDeleteOpen, setBulkDeleteOpen] = useState(false)
  const [confirmArmed, setConfirmArmed] = useState(false)
  useEffect(() => {
    if (!bulkDeleteOpen) {
      setConfirmArmed(false)
      return
    }
    const timer = window.setTimeout(() => setConfirmArmed(true), selectedIds.length >= 5 ? 2000 : 0)
    return () => window.clearTimeout(timer)
  }, [bulkDeleteOpen, selectedIds.length])

  /** weight 行内快编的编辑态（单行） */
  const [editingWeightId, setEditingWeightId] = useState<number | null>(null)
  const [weightDraft, setWeightDraft] = useState("")

  const startWeightEdit = (association: ModelWithProvider) => {
    setEditingWeightId(association.ID)
    setWeightDraft(String(association.Weight))
  }

  const commitWeightEdit = async (association: ModelWithProvider) => {
    // Enter 提交后 unmount、blur 可能不再派发，但历史行为有分歧——入口
    // 守卫挡住第二次调用（行数据尚未回填时会重复 PUT）
    if (editingWeightId !== association.ID) return
    setEditingWeightId(null)
    const parsed = parseInt(weightDraft, 10)
    if (Number.isNaN(parsed) || parsed < 1 || parsed === association.Weight) return
    await onWeightSave(association, parsed)
  }

  /** 四个能力列的取值方式一致：支持/不支持 */
  const capability = (ok: boolean) => (
    <Mark ok={ok} label={t(ok ? "association_table.supported" : "association_table.unsupported")} />
  )

  /**
   * 近期成败条的读数。
   *
   * 一排绿/红小格是纯颜色信息：色盲用户读不出，读屏用户更是什么都读不到
   * （条本身只挂了 hover 才出现的 title）。因此小格只作装饰，含义由旁边这行
   * 计数文字承担。
   */
  const recentSummary = (statusBars: boolean[]) =>
    t("association_table.recent_summary", {
      success: statusBars.filter(Boolean).length,
      total: statusBars.length,
    })

  /**
   * 删除确认的内容。桌面表格与手机卡片各挂一次（一个靠 Trigger 打开、
   * 一个靠外层 open 控制），文案只此一份——原先两边各写一遍，改一处漏一处。
   */
  const deleteConfirm = (
    <AlertDialogContent>
      <AlertDialogHeader>
        <AlertDialogTitle>{t("association_table.delete_title")}</AlertDialogTitle>
        <AlertDialogDescription>{t("association_table.delete_desc")}</AlertDialogDescription>
      </AlertDialogHeader>
      <AlertDialogFooter>
        <AlertDialogCancel onClick={onCloseDelete}>{t("common:actions.cancel")}</AlertDialogCancel>
        <AlertDialogAction onClick={onConfirmDelete}>{t("common:actions.confirm_delete")}</AlertDialogAction>
      </AlertDialogFooter>
    </AlertDialogContent>
  )

  return (
    <div className="flex-1 min-h-0 border rounded-md bg-background shadow-sm">
      {error ? (
        <div className="p-3">
          <ErrorState
            title={t("load_failed_associations")}
            message={error}
            retryLabel={t("retry")}
            onRetry={onRetry}
          />
        </div>
      ) : loading ? (
        <div className="flex h-full items-center justify-center">
          <Loading message={t("loading_associations")} />
        </div>
      ) : associations.length === 0 ? (
        <div className="flex h-full items-center justify-center text-muted-foreground text-sm text-center px-6">
          {emptyText}
        </div>
      ) : (
        <div className="h-full flex flex-col">
          {/* 批量工具栏（桌面）。选中数用 aria-live 播报——视觉浮现的
              工具栏对读屏用户不产生任何事件。 */}
          {(selectedIds.length > 0 || bulkTestProgress) && (
            <div className="hidden sm:flex items-center gap-2 border-b px-3 py-2 flex-shrink-0">
              <span className="text-xs text-muted-foreground" aria-live="polite">
                {t("association_table.bulk.selected", { count: selectedIds.length })}
              </span>
              {bulkTestProgress ? (
                <span className="text-xs text-muted-foreground">
                  {t("association_table.bulk.testing", {
                    done: bulkTestProgress.done,
                    total: bulkTestProgress.total,
                  })}
                </span>
              ) : (
                <>
                  <Button
                    variant="outline" size="sm" className="h-7 text-xs"
                    disabled={bulkPending}
                    onClick={() => onBulkStatus(true)}
                  >
                    {t("association_table.bulk.enable")}
                  </Button>
                  <Button
                    variant="outline" size="sm" className="h-7 text-xs"
                    disabled={bulkPending}
                    onClick={() => onBulkStatus(false)}
                  >
                    {t("association_table.bulk.disable")}
                  </Button>
                  {/* 批量测试是真实计费请求：N 条就是向上游打 N 发，
                      确认文案必须把这笔账说在按下去之前。 */}
                  <AlertDialog>
                    <AlertDialogTrigger asChild>
                      <Button
                        variant="outline" size="sm" className="h-7 text-xs"
                        disabled={bulkPending}
                      >
                        <Zap className="size-3.5" aria-hidden="true" />
                        {t("association_table.bulk.test")}
                      </Button>
                    </AlertDialogTrigger>
                    <AlertDialogContent>
                      <AlertDialogHeader>
                        <AlertDialogTitle>
                          {t("association_table.bulk.test_title", { count: selectedIds.length })}
                        </AlertDialogTitle>
                        <AlertDialogDescription>
                          {t("association_table.bulk.test_desc", { count: selectedIds.length })}
                        </AlertDialogDescription>
                      </AlertDialogHeader>
                      <AlertDialogFooter>
                        <AlertDialogCancel>{t("common:actions.cancel")}</AlertDialogCancel>
                        <AlertDialogAction onClick={() => onBulkTest(selectedIds)}>
                          {t("association_table.bulk.test_confirm")}
                        </AlertDialogAction>
                      </AlertDialogFooter>
                    </AlertDialogContent>
                  </AlertDialog>
                  {/* 补全元数据：0 价档位会被数据源价覆盖——这句必须出现在
                      按下去之前，而不是事后报告里 */}
                  <AlertDialog>
                    <AlertDialogTrigger asChild>
                      <Button variant="outline" size="sm" className="h-7 text-xs" disabled={bulkPending}>
                        {t("association_table.bulk.backfill")}
                      </Button>
                    </AlertDialogTrigger>
                    <AlertDialogContent>
                      <AlertDialogHeader>
                        <AlertDialogTitle>{t("association_table.bulk.backfill_title", { count: selectedIds.length })}</AlertDialogTitle>
                        <AlertDialogDescription>
                          {t("association_table.bulk.backfill_desc")}
                        </AlertDialogDescription>
                      </AlertDialogHeader>
                      <AlertDialogFooter>
                        <AlertDialogCancel>{t("common:actions.cancel")}</AlertDialogCancel>
                        <AlertDialogAction onClick={() => onBackfill(selectedIds)}>
                          {t("association_table.bulk.backfill_confirm")}
                        </AlertDialogAction>
                      </AlertDialogFooter>
                    </AlertDialogContent>
                  </AlertDialog>
                  <AlertDialog open={bulkDeleteOpen} onOpenChange={setBulkDeleteOpen}>
                    <AlertDialogTrigger asChild>
                      <Button
                        variant="destructive" size="sm" className="h-7 text-xs"
                        disabled={bulkPending}
                        onClick={() => setBulkDeleteOpen(true)}
                      >
                        <Trash2 className="size-3.5" aria-hidden="true" />
                        {t("common:actions.delete")}
                      </Button>
                    </AlertDialogTrigger>
                    <AlertDialogContent>
                      <AlertDialogHeader>
                        <AlertDialogTitle>
                          {t("association_table.bulk.delete_title", { count: selectedIds.length })}
                        </AlertDialogTitle>
                        <AlertDialogDescription asChild>
                          <div className="space-y-2">
                            <p>
                              {t("association_table.bulk.delete_desc", { count: selectedIds.length })}
                            </p>
                            <div className="max-h-32 overflow-y-auto rounded-md border px-2 py-1 text-xs font-mono">
                              {[...new Set(
                                associations
                                  .filter((a) => selectedIdSet.has(a.ID))
                                  .map((a) => a.ProviderModel),
                              )].map((name) => (
                                <div key={name}>{name}</div>
                              ))}
                            </div>
                            {selectedIds.length >= 5 && (
                              <p className="text-xs text-muted-foreground">
                                {t("association_table.bulk.delete_delay_hint")}
                              </p>
                            )}
                          </div>
                        </AlertDialogDescription>
                      </AlertDialogHeader>
                      <AlertDialogFooter>
                        <AlertDialogCancel disabled={bulkPending}>
                          {t("common:actions.cancel")}
                        </AlertDialogCancel>
                        <AlertDialogAction
                          disabled={!confirmArmed || bulkPending}
                          onClick={() => onBulkDelete(selectedIds)}
                        >
                          {t("common:actions.confirm_delete")}
                        </AlertDialogAction>
                      </AlertDialogFooter>
                    </AlertDialogContent>
                  </AlertDialog>
                </>
              )}
            </div>
          )}
          <div className="hidden sm:block flex-1 overflow-y-auto">
            <div className="w-full">
              <Table className="min-w-[1200px]">
                <TableHeader className="z-10 sticky top-0 bg-secondary/80 text-secondary-foreground">
                  <TableRow>
                    <TableHead className="w-10">
                      <Checkbox
                        checked={allVisibleSelected ? true : selectedIds.length > 0 ? "indeterminate" : false}
                        onCheckedChange={onToggleSelectAll}
                        aria-label={t("association_table.bulk.select_all")}
                      />
                    </TableHead>
                    <TableHead>{t("association_table.id")}</TableHead>
                    <TableHead>{t("association_table.provider_model")}</TableHead>
                    <TableHead>{t("association_table.type")}</TableHead>
                    <TableHead>{t("association_table.provider")}</TableHead>
                    <TableHead>{t("association_table.tool_call")}</TableHead>
                    <TableHead>{t("association_table.structured_output")}</TableHead>
                    <TableHead>{t("association_table.vision")}</TableHead>
                    <TableHead>{t("association_table.with_header")}</TableHead>
                    <TableHead>{t("association_table.weight")}</TableHead>
                    <TableHead>{t("association_table.enabled")}</TableHead>
                    <TableHead>
                      <div className="flex items-center gap-1">{t("association_table.status")}
                        <Button
                          onClick={onRefreshStatus}
                          variant="ghost"
                          size="icon"
                          aria-label={t("actions.refresh_status")}
                          title={t("actions.refresh_status")}
                          className="rounded-full"
                        >
                          <RefreshCw className="size-4" />
                        </Button>
                      </div>
                    </TableHead>
                    <TableHead>{t("association_table.actions")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {associations.map((association) => {
                    const provider = providers.find(p => p.ID === association.ProviderID);
                    const isAssociationEnabled = association.Status ?? false;
                    const statusBars = providerStatus[association.ID];
                    return (
                      <TableRow key={association.ID}>
                        <TableCell className="w-10">
                          <Checkbox
                            checked={selectedIdSet.has(association.ID)}
                            onCheckedChange={() => onToggleSelect(association.ID)}
                            aria-label={t("association_table.bulk.select_row", {
                              name: association.ProviderModel,
                            })}
                          />
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">{association.ID}</TableCell>
                        <TableCell className="max-w-[200px] truncate" title={association.ProviderModel}>
                          {association.ProviderModel}
                        </TableCell>
                        <TableCell>{provider?.Type ?? t("common:unknown")}</TableCell>
                        <TableCell>{provider?.Name ?? t("common:unknown")}</TableCell>
                        <TableCell>{capability(association.ToolCall)}</TableCell>
                        <TableCell>{capability(association.StructuredOutput)}</TableCell>
                        <TableCell>{capability(association.Image)}</TableCell>
                        <TableCell>{capability(association.WithHeader)}</TableCell>
                        <TableCell>
                          {/* weight 行内快编：批量建完后微调权重不该每次都开
                              15 字段的全量弹窗。双击进入编辑，Enter/失焦提交。 */}
                          {editingWeightId === association.ID ? (
                            <Input
                              autoFocus
                              type="number"
                              min="1"
                              className="h-7 w-16 text-xs"
                              value={weightDraft}
                              onChange={(e) => setWeightDraft(e.target.value)}
                              onBlur={() => void commitWeightEdit(association)}
                              onKeyDown={(e) => {
                                if (e.key === "Enter") void commitWeightEdit(association)
                                if (e.key === "Escape") setEditingWeightId(null)
                              }}
                            />
                          ) : (
                            <span
                              className="cursor-text tabular-nums"
                              title={t("association_table.weight_edit_hint")}
                              onDoubleClick={() => startWeightEdit(association)}
                            >
                              {association.Weight}
                            </span>
                          )}
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-2">
                            <Switch
                              checked={isAssociationEnabled}
                              disabled={!!statusUpdating[association.ID]}
                              onCheckedChange={(value) => onStatusToggle(association, value)}
                              aria-label={t("association_table.toggle_status")}
                            />
                            <span className="text-xs text-muted-foreground">
                              {isAssociationEnabled ? t("association_table.active") : t("association_table.inactive")}
                            </span>
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-col gap-1 w-max">
                            {statusBars ? (
                              statusBars.length > 0 ? (
                                <>
                                  <div className="flex space-x-1 items-end h-4" aria-hidden="true">
                                    {statusBars.map((isSuccess, index) => (
                                      <div
                                        key={index}
                                        className={`w-1 h-4 rounded-sm ${isSuccess ? "bg-status-good" : "bg-status-critical"}`}
                                        title={isSuccess ? t("association_table.success") : t("association_table.failed")}
                                      />
                                    ))}
                                  </div>
                                  <span className="text-[10px] text-muted-foreground tabular-nums">
                                    {recentSummary(statusBars)}
                                  </span>
                                </>
                              ) : (
                                <div className="text-xs text-muted-foreground">{t("association_table.no_data")}</div>
                              )
                            ) : (
                              <Spinner />
                            )}
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-wrap gap-2">
                            <Button
                              variant="outline"
                              size="icon"
                              aria-label={t("common:actions.edit")}
                              onClick={() => onEdit(association)}
                            >
                              <Pencil className="h-4 w-4" aria-hidden="true" />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              aria-label={t("common:actions.test")}
                              onClick={() => onTest(association.ID)}
                            >
                              <Zap className="h-4 w-4" aria-hidden="true" />
                            </Button>
                            <AlertDialog open={deleteId === association.ID} onOpenChange={(open) => !open && onCloseDelete()}>
                              <AlertDialogTrigger asChild>
                                <Button
                                  variant="destructive"
                                  size="icon"
                                  aria-label={t("common:actions.delete")}
                                  onClick={() => onOpenDelete(association.ID)}
                                >
                                  <Trash2 className="h-4 w-4" aria-hidden="true" />
                                </Button>
                              </AlertDialogTrigger>
                              {deleteConfirm}
                            </AlertDialog>
                          </div>
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          </div>
          <div className="sm:hidden flex-1 min-h-0 overflow-y-auto px-2 py-3 divide-y divide-border">
            {associations.map((association) => {
              const provider = providers.find(p => p.ID === association.ProviderID);
              const isAssociationEnabled = association.Status ?? true;
              const statusBars = providerStatus[association.ID];
              return (
                <div key={association.ID} className="py-3 space-y-3">
                  <div className="flex items-start justify-between gap-2">
                    <div className="min-w-0 flex-1">
                      <h3 className="font-semibold text-sm truncate">{provider?.Name ?? t("association_table.unknown_provider")}</h3>
                      <p className="text-[11px] text-muted-foreground">{t("association_table.mobile.provider_type")}: {association.ProviderModel}</p>
                    </div>
                    <span
                      className={`text-[11px] font-medium px-2 py-0.5 rounded-full ${isAssociationEnabled ? "bg-status-good/10 text-status-good-ink" : "bg-status-critical/10 text-status-critical-ink"}`}
                    >
                      {isAssociationEnabled ? t("association_table.active") : t("association_table.inactive")}
                    </span>
                  </div>
                  <div className="grid grid-cols-2 gap-3 text-xs">
                    <MobileInfoItem label={t("association_table.mobile.provider_type")} value={provider?.Type ?? t("common:unknown")} />
                    <MobileInfoItem label={t("association_table.mobile.provider_id")} value={<span className="font-mono text-xs">{provider?.ID ?? "-"}</span>} />
                    <MobileInfoItem label={t("association_table.mobile.weight")} value={association.Weight} />
                    <MobileInfoItem
                      label={t("association_table.mobile.with_header")}
                      value={capability(association.WithHeader)}
                    />
                  </div>
                  <div className="grid grid-cols-2 gap-3 text-xs">
                    <MobileInfoItem
                      label={t("association_table.mobile.tool_call")}
                      value={capability(association.ToolCall)}
                    />
                    <MobileInfoItem
                      label={t("association_table.mobile.structured_output")}
                      value={capability(association.StructuredOutput)}
                    />
                    <MobileInfoItem
                      label={t("association_table.mobile.vision")}
                      value={capability(association.Image)}
                    />
                    <MobileInfoItem
                      label={t("association_table.mobile.recent_status")}
                      value={
                        <div className="flex items-center gap-1.5">
                          {statusBars ? (
                            statusBars.length > 0 ? (
                              <>
                                <div className="flex items-center gap-0.5" aria-hidden="true">
                                  {statusBars.map((isSuccess, index) => (
                                    <div
                                      key={index}
                                      className={`w-1 h-4 rounded ${isSuccess ? "bg-status-good" : "bg-status-critical"}`}
                                    />
                                  ))}
                                </div>
                                <span className="text-[11px] text-muted-foreground tabular-nums">
                                  {recentSummary(statusBars)}
                                </span>
                              </>
                            ) : (
                              <span className="text-muted-foreground text-[11px]">{t("association_table.no_data")}</span>
                            )
                          ) : (
                            <Spinner />
                          )}
                        </div>
                      }
                    />
                  </div>
                  <div className="flex items-center justify-between rounded-md border bg-muted/30 px-3 py-2">
                    <p className="text-xs text-muted-foreground">{t("association_table.mobile.enable_status")}</p>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{isAssociationEnabled ? t("association_table.active") : t("association_table.inactive")}</span>
                      <Switch
                        checked={isAssociationEnabled}
                        disabled={!!statusUpdating[association.ID]}
                        onCheckedChange={(value) => onStatusToggle(association, value)}
                        aria-label={t("association_table.toggle_status")}
                      />
                    </div>
                  </div>
                  <div className="flex flex-wrap justify-end gap-1.5">
                    <Button
                      variant="outline"
                      size="icon"
                      className="h-7 w-7"
                      aria-label={t("common:actions.edit")}
                      onClick={() => onEdit(association)}
                    >
                      <Pencil className="h-3.5 w-3.5" aria-hidden="true" />
                    </Button>
                    <Button
                      variant="outline"
                      size="icon"
                      className="h-7 w-7"
                      aria-label={t("common:actions.test")}
                      onClick={() => onTest(association.ID)}
                    >
                      <Zap className="h-3.5 w-3.5" aria-hidden="true" />
                    </Button>
                    <AlertDialog open={deleteId === association.ID} onOpenChange={(open) => !open && onCloseDelete()}>
                      <Button
                        variant="destructive"
                        size="icon"
                        className="h-7 w-7"
                        aria-label={t("common:actions.delete")}
                        onClick={() => onOpenDelete(association.ID)}
                      >
                        <Trash2 className="h-3.5 w-3.5" aria-hidden="true" />
                      </Button>
                      {deleteConfirm}
                    </AlertDialog>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  )
}
