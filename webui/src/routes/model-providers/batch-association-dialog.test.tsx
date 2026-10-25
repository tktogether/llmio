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
function Harness({ lockedModelId = null }: { lockedModelId?: number | null }) {
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
      onCompleted={vi.fn()}
    />
  )
}

async function openDialog(user: ReturnType<typeof userEvent.setup>) {
  render(<Harness />)
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
    // 只有能对上同名的进了提交，孤儿不进
    expect(req.dry_run).toBe(true)
    expect(req.model_ids).toEqual([1])
    expect(req.provider_models).toEqual(["gpt-test"])
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
