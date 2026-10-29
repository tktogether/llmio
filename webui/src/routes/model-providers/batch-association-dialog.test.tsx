import { render, screen, waitFor } from "@testing-library/react"
import { userEvent } from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"
import { useState } from "react"

import { BatchAssociationDialog } from "@/routes/model-providers/batch-association-dialog"
import { batchCreateModelProviders, getProviderModels, type Model, type Provider, type ProviderModel } from "@/lib/api"

// 弹窗自己的 IO 只有两个函数：创建走批量端点，模型列表走供应商拉取。
// 策略读取不在这个组件里（批量预填在服务端做），所以工厂比页面测试小得多。
vi.mock("@/lib/api", () => ({
  batchCreateModelProviders: vi.fn(),
  getProviderModels: vi.fn(),
}))

const mocked = {
  batchCreate: vi.mocked(batchCreateModelProviders),
  getProviderModels: vi.mocked(getProviderModels),
}

function model(over: Partial<Model> = {}): Model {
  return { ID: 1, Name: "gpt-test", Remark: "", MaxRetry: 3, TimeOut: 300, Strategy: "lottery", Breaker: false, DisplayOrder: 1, ...over }
}

function provider(over: Partial<Provider> = {}): Provider {
  return { ID: 11, Name: "十一号上游", Type: "openai", Config: "", Console: "", Proxy: "", ErrorMatcher: "", ...over }
}

function pm(id: string): ProviderModel {
  return { id, object: "model", created: 0, owned_by: "" }
}

/** 模拟页面对弹窗的数据供给：loadProviderModels 真调 api 并回填 map——
 *  弹窗的列表完全来自 props，静态 map 测不出"选供应商后列表出现"。 */
function Harness({ lockedModelId = null, onCompleted }: { lockedModelId?: number | null; onCompleted?: (ids: number[]) => void | Promise<void> }) {
  const [map, setMap] = useState<Record<number, ProviderModel[]>>({})
  const loadProviderModels = vi.fn(async (id: number) => {
    const data = await getProviderModels(id)
    setMap((prev) => ({ ...prev, [id]: data }))
  })
  return (
    <BatchAssociationDialog
      open
      onOpenChange={vi.fn()}
      models={[model({ ID: 1, Name: "gpt-test" }), model({ ID: 2, Name: "claude-test" })]}
      providers={[provider()]}
      providerModelsMap={map}
      providerModelsLoading={{}}
      loadProviderModels={loadProviderModels}
      lockedModelId={lockedModelId}
      onCompleted={onCompleted ?? vi.fn()}
    />
  )
}

async function openDialog(user: ReturnType<typeof userEvent.setup>, onCompleted?: (ids: number[]) => void | Promise<void>) {
  render(<Harness onCompleted={onCompleted} />)
  await screen.findByRole("dialog")
  // 选供应商 → 触发一次列表加载。弹窗里有多个 combobox（供应商/币种/三能力），
  // 供应商那个在 DOM 最前。
  await user.click(screen.getAllByRole("combobox")[0])
  await user.click(await screen.findByRole("option", { name: "十一号上游" }))
  await waitFor(() => expect(mocked.getProviderModels).toHaveBeenCalled())
}

beforeEach(async () => {
  vi.clearAllMocks()
  const i18n = (await import("@/i18n")).default
  await i18n.changeLanguage("zh-CN")
})

describe("批量关联弹窗 · 同名挂载", () => {
  it("选中的上游模型逐一标注同名目标；没有同名的明说并从提交里排除", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test"), pm("orphan-model")])
    await openDialog(user)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("checkbox", { name: /orphan-model/ }))

    expect(screen.getByText(/同名模型 #1/)).toBeInTheDocument()
    expect(screen.getByText(/无同名逻辑模型/)).toBeInTheDocument()

    mocked.batchCreate.mockResolvedValue({
      dry_run: true, created: [], skipped: [], failed: [], unfilled: [],
    })
    await user.click(screen.getByRole("button", { name: "预览" }))

    await waitFor(() => expect(mocked.batchCreate).toHaveBeenCalledTimes(1))
    const req = mocked.batchCreate.mock.calls[0][0]
    // 只有能对上同名的进了提交（pairs 逐对），孤儿不进
    expect(req.dry_run).toBe(true)
    expect(req.pairs).toEqual([{ model_id: 1, provider_model: "gpt-test" }])
    expect(req.model_ids).toEqual([])
    expect(req.provider_models).toEqual([])
  })

  it("预览→确认两步走：确认才真正写入，且请求体不带 dry_run", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    await openDialog(user)
    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))

    mocked.batchCreate.mockResolvedValueOnce({
      dry_run: true,
      created: [
        { ID: 0, ModelID: 1, ProviderModel: "gpt-test", Weight: 1, Currency: "CNY" } as never,
      ],
      skipped: [], failed: [], unfilled: [],
    })
    await user.click(screen.getByRole("button", { name: "预览" }))
    expect(await screen.findByText(/将新建 1 条/)).toBeInTheDocument()

    mocked.batchCreate.mockResolvedValueOnce({
      dry_run: false,
      created: [
        { ID: 77, ModelID: 1, ProviderModel: "gpt-test", Weight: 1, Currency: "USD",
          Autofill: { source: "models.dev", filled: { input_price: 3, tool_call: true } } } as never,
      ],
      skipped: [
        { model_id: 2, provider_model: "gpt-test", reason: "exists", existing_status: false },
      ],
      failed: [],
      unfilled: [{ provider_model: "gpt-test", reason: "no_model_match" }],
    })
    await user.click(screen.getByRole("button", { name: /创建 1 条/ }))

    await waitFor(() => expect(mocked.batchCreate).toHaveBeenCalledTimes(2))
    expect(mocked.batchCreate.mock.calls[1][0].dry_run).toBe(false)

    // 结果面板：补全摘要、已存在停用态、未补全原因都看得见
    expect(await screen.findByText(/元数据补全（models.dev）/)).toBeInTheDocument()
    expect(screen.getByText(/已存在且处于停用状态/)).toBeInTheDocument()
    expect(screen.getByText(/数据源没有该模型/)).toBeInTheDocument()
  })

  it("确认写入成功后立即通知页面刷新，不等弹窗关闭", async () => {
    const user = userEvent.setup()
    const onCompleted = vi.fn()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    await openDialog(user, onCompleted)
    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))

    mocked.batchCreate.mockResolvedValueOnce({
      dry_run: true,
      created: [
        { ID: 0, ModelID: 1, ProviderModel: "gpt-test", Weight: 1, Currency: "CNY" } as never,
      ],
      skipped: [], failed: [], unfilled: [],
    })
    await user.click(screen.getByRole("button", { name: "预览" }))
    expect(await screen.findByText(/将新建 1 条/)).toBeInTheDocument()
    // 预览（dry_run）不是写入，不该惊动页面
    expect(onCompleted).not.toHaveBeenCalled()

    mocked.batchCreate.mockResolvedValueOnce({
      dry_run: false,
      created: [
        { ID: 77, ModelID: 1, ProviderModel: "gpt-test", Weight: 1, Currency: "CNY" } as never,
      ],
      skipped: [], failed: [], unfilled: [],
    })
    await user.click(screen.getByRole("button", { name: /创建 1 条/ }))

    // 结果面板一出现，刷新就已发生——用户停在结果面板时页面数据是最新的
    expect(await screen.findByText(/新建 1 条/)).toBeInTheDocument()
    expect(onCompleted).toHaveBeenCalledTimes(1)
    expect(onCompleted).toHaveBeenCalledWith([1])
  })
})

describe("批量关联弹窗 · 粘贴兜底", () => {
  it("列表拉不到时切粘贴模式，两个方向的选区同步", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([])
    await openDialog(user)

    // 拉到空列表：提示手填 + 手动添加入口
    expect(await screen.findByText(/请手动添加模型名/)).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: "粘贴名单" }))
    const area = screen.getByPlaceholderText(/每行一个上游模型名/)
    await user.type(area, "alpha\nbeta")

    expect(screen.getByText(/已选 2 个/)).toBeInTheDocument()

    // 切回列表模式：手动名以 chips 呈现，不丢（同名挂载的映射列表里
    // 也会出现一次，所以断言"至少还在"而不是恰好一次）
    await user.click(screen.getByRole("button", { name: "回到列表" }))
    expect(screen.getAllByText("alpha").length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText("beta").length).toBeGreaterThanOrEqual(1)
  })
})

describe("批量关联弹窗 · 同名挂载发 pairs（不是笛卡尔）", () => {
  it("双选同名模型：请求是逐对 pairs，model_ids/provider_models 为空", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test"), pm("claude-test")])
    await openDialog(user)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("checkbox", { name: /claude-test/ }))

    mocked.batchCreate.mockResolvedValue({
      dry_run: true, created: [], skipped: [], failed: [], unfilled: [],
    })
    await user.click(screen.getByRole("button", { name: "预览" }))

    await waitFor(() => expect(mocked.batchCreate).toHaveBeenCalledTimes(1))
    const req = mocked.batchCreate.mock.calls[0][0]
    // N 个同名模型若发笛卡尔会建 N×N 条（N²−N 条交叉行）；pairs 逐对建立
    expect(req.model_ids).toEqual([])
    expect(req.provider_models).toEqual([])
    // 映射列表按上游模型名排序，claude-test 在前
    expect(req.pairs).toEqual([
      { model_id: 2, provider_model: "claude-test" },
      { model_id: 1, provider_model: "gpt-test" },
    ])
  })

  it("预览后改配置，预览与确认入口一并失效（不留'改之前的答案'）", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    await openDialog(user)
    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))

    mocked.batchCreate.mockResolvedValue({
      dry_run: true,
      created: [{ ID: 0, ModelID: 1, ProviderModel: "gpt-test" } as never],
      skipped: [], failed: [], unfilled: [],
    })
    await user.click(screen.getByRole("button", { name: "预览" }))
    expect(await screen.findByText(/将新建 1 条/)).toBeInTheDocument()

    // 改 weight：预览面板与"创建 N 条"按钮必须消失
    const weight = screen.getByRole("spinbutton", { name: /权重/ })
    await user.clear(weight)
    await user.type(weight, "5")

    await waitFor(() => expect(screen.queryByText(/将新建 1 条/)).not.toBeInTheDocument())
    expect(screen.queryByRole("button", { name: /创建 1 条/ })).not.toBeInTheDocument()
  })

  it("失败重试从提交快照构造 pairs，不重算笛卡尔、不吃表单后续编辑", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    await openDialog(user)
    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))

    mocked.batchCreate.mockImplementation(async (req: { dry_run?: boolean }) => {
      if (req.dry_run) {
        return { dry_run: true, created: [{ ID: 0, ModelID: 1, ProviderModel: "gpt-test" } as never], skipped: [], failed: [], unfilled: [] }
      }
      return {
        dry_run: false, created: [],
        failed: [{ model_id: 1, provider_model: "gpt-test", reason: "db: boom" }],
        skipped: [], unfilled: [],
      }
    })
    await user.click(screen.getByRole("button", { name: "预览" }))
    await user.click(await screen.findByRole("button", { name: /创建 1 条/ }))
    expect(await screen.findByText(/db: boom/)).toBeInTheDocument()

    // 提交后改 weight——重试请求仍应带提交时的 weight=1，且 pairs 精确等于失败对
    const weight = screen.getByRole("spinbutton", { name: /权重/ })
    await user.clear(weight)
    await user.type(weight, "9")

    mocked.batchCreate.mockResolvedValue({
      dry_run: false, created: [{ ID: 8, ModelID: 1, ProviderModel: "gpt-test" } as never],
      skipped: [], failed: [], unfilled: [],
    })
    await user.click(screen.getByRole("button", { name: /重试失败的 1 条/ }))

    await waitFor(() => {
      const calls = mocked.batchCreate.mock.calls
      const retry = calls[calls.length - 1][0]
      expect(retry.dry_run).toBe(false)
      expect(retry.defaults.weight).toBe(1) // 快照，不是表单当前值
      expect(retry.pairs).toEqual([{ model_id: 1, provider_model: "gpt-test" }])
      expect(retry.model_ids).toEqual([])
    })
  })
})
