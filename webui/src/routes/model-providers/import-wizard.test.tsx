import { render, screen, waitFor } from "@testing-library/react"
import { userEvent } from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"
import { useState } from "react"

import { ImportWizard } from "@/routes/model-providers/import-wizard"
import {
  batchCreateModelProviders,
  batchCreateModels,
  getModelMetadata,
  getModelProviders,
  getProviderModels,
  type Model,
  type Provider,
  type ProviderModel,
} from "@/lib/api"

// 向导的两步编排全部经 api.ts：第一步建模型、第二步 pairs 挂关联。
vi.mock("@/lib/api", () => ({
  batchCreateModelProviders: vi.fn(),
  batchCreateModels: vi.fn(),
  getModelMetadata: vi.fn(),
  getModelProviders: vi.fn(),
  getProviderModels: vi.fn(),
}))

const mocked = {
  batchCreateModelProviders: vi.mocked(batchCreateModelProviders),
  batchCreateModels: vi.mocked(batchCreateModels),
  getModelMetadata: vi.mocked(getModelMetadata),
  getModelProviders: vi.mocked(getModelProviders),
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

function Harness({ models, onCompleted }: { models: Model[]; onCompleted?: () => void | Promise<void> }) {
  const [map, setMap] = useState<Record<number, ProviderModel[]>>({})
  const loadProviderModels = vi.fn(async (id: number) => {
    const data = await getProviderModels(id)
    setMap((prev) => ({ ...prev, [id]: data }))
  })
  return (
    <ImportWizard
      open
      onOpenChange={vi.fn()}
      models={models}
      providers={[provider()]}
      providerModelsMap={map}
      providerModelsLoading={{}}
      loadProviderModels={loadProviderModels}
      onCompleted={onCompleted ?? vi.fn()}
    />
  )
}

async function openWizard(user: ReturnType<typeof userEvent.setup>, onCompleted?: () => void | Promise<void>) {
  render(<Harness models={[model({ ID: 1, Name: "gpt-test" })]} onCompleted={onCompleted} />)
  await screen.findByRole("dialog")
  await user.click(screen.getAllByRole("combobox")[0])
  await user.click(await screen.findByRole("option", { name: "十一号上游" }))
  await waitFor(() => expect(mocked.getProviderModels).toHaveBeenCalled())
}

beforeEach(async () => {
  vi.clearAllMocks()
  mocked.getModelMetadata.mockResolvedValue({
    matched: false, reason: "no_provider_match",
    tool_call: null, structured_output: null, image: null,
    input_price: null, cache_read_price: null, output_price: null,
  })
  // 熔断建议会拉挂载目标的既有关联；默认空（多数用例不关心这路 IO）
  mocked.getModelProviders.mockResolvedValue([])
  const i18n = (await import("@/i18n")).default
  await i18n.changeLanguage("zh-CN")
})

describe("从供应商导入 · 两步编排", () => {
  it("新建走 models/batch、挂关联走 pairs，不产生交叉", async () => {
    const user = userEvent.setup()
    // gpt-test 已存在（attach）；fresh-model 不存在（create）
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test"), pm("fresh-model")])
    // 先备好实现：进入差异页的瞬间，attach 预查（dry_run）就会发出
    mocked.batchCreateModelProviders.mockResolvedValue({
      dry_run: true,
      created: [], skipped: [], failed: [], unfilled: [],
    })
    mocked.batchCreateModels.mockResolvedValue({
      created: [model({ ID: 9, Name: "fresh-model" })],
      skipped: [],
    })
    await openWizard(user)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("checkbox", { name: /fresh-model/ }))
    await user.click(screen.getByRole("button", { name: "下一步" }))

    // 差异页把两类分开说
    expect(await screen.findByText(/将新建逻辑模型并挂关联：1 个/)).toBeInTheDocument()
    expect(screen.getByText(/将挂到已有同名逻辑模型：1 个/)).toBeInTheDocument()
    expect(screen.getByText(/立即出现在客户端 \/v1\/models/)).toBeInTheDocument()

    // 真导入那次返回空结果（dry_run=false 走默认实现之外的这条）
    mocked.batchCreateModelProviders.mockImplementation(async (_req: unknown) => {
      const req = _req as { dry_run?: boolean }
      if (req.dry_run) {
        return { dry_run: true, created: [], skipped: [], failed: [], unfilled: [] }
      }
      return { dry_run: false, created: [], skipped: [], failed: [], unfilled: [] }
    })
    await user.click(screen.getByRole("button", { name: /导入 2 个/ }))

    await waitFor(() => expect(mocked.batchCreateModels).toHaveBeenCalledTimes(1))
    expect(mocked.batchCreateModels.mock.calls[0][0]).toEqual(["fresh-model"])

    await waitFor(() => expect(mocked.batchCreateModelProviders).toHaveBeenCalledTimes(2)) // 1 次 dry_run 预查 + 1 次真导入
    const realCall = mocked.batchCreateModelProviders.mock.calls.find(
      ([req]) => !(req as { dry_run?: boolean }).dry_run,
    )
    expect(realCall).toBeDefined()
    const pairs = (realCall![0] as unknown as { pairs: { model_id: number; provider_model: string }[] }).pairs
    // pairs 逐对：已有的挂 id 1，新建的挂 id 9——没有任何交叉组合
    expect(pairs).toEqual(
      expect.arrayContaining([
        { model_id: 1, provider_model: "gpt-test" },
        { model_id: 9, provider_model: "fresh-model" },
      ]),
    )
    expect(pairs).toHaveLength(2)

    expect(await screen.findByText(/新建 0 条，跳过 0 条，失败 0 条/)).toBeInTheDocument()
  })

  it("元数据源未就绪时，差异页在下单前把话说出来", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("fresh-model")])
    mocked.getModelMetadata.mockResolvedValue({
      matched: false, reason: "catalog_unavailable",
      tool_call: null, structured_output: null, image: null,
      input_price: null, cache_read_price: null, output_price: null,
    })
    await openWizard(user)

    await user.click(await screen.findByRole("checkbox", { name: /fresh-model/ }))
    await user.click(screen.getByRole("button", { name: "下一步" }))

    expect(await screen.findByText(/元数据源尚未就绪/)).toBeInTheDocument()
  })
})

describe("从供应商导入 · attach 开关", () => {
  it("取消勾选后：开关还在（可再勾回），导入不含已有同名模型的配对", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    mocked.batchCreateModelProviders.mockResolvedValue({
      dry_run: true, created: [], skipped: [], failed: [], unfilled: [],
    })
    mocked.batchCreateModels.mockResolvedValue({ created: [], skipped: [] })
    await openWizard(user)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("button", { name: "下一步" }))

    // 差异页出现 attach 开关；取消勾选后开关仍在（单向门修复）
    const attachToggle = await screen.findByRole("checkbox")
    await user.click(attachToggle)
    expect(screen.getByRole("checkbox")).toBeInTheDocument()

    mocked.batchCreateModelProviders.mockImplementation(async (req: unknown) => {
      const r = req as { dry_run?: boolean }
      if (r.dry_run) return { dry_run: true, created: [], skipped: [], failed: [], unfilled: [] }
      return { dry_run: false, created: [], skipped: [], failed: [], unfilled: [] }
    })
    await user.click(screen.getByRole("button", { name: /导入 1 个/ }))

    // 唯一一项是同名已有模型且被策略排除 → 第二步整体跳过：
    // 不发真导入调用，直接进结果页
    await waitFor(() => {
      const realCalls = mocked.batchCreateModelProviders.mock.calls.filter(
        ([req]) => !(req as { dry_run?: boolean }).dry_run,
      )
      expect(realCalls).toEqual([])
    })
    expect(await screen.findByText("导入结果")).toBeInTheDocument()
  })

  it("取消 attach 后摘要计数跟着归零，不再与下方清单矛盾", async () => {
    const user = userEvent.setup()
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    mocked.batchCreateModelProviders.mockResolvedValue({
      dry_run: true, created: [], skipped: [], failed: [], unfilled: [],
    })
    await openWizard(user)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("button", { name: "下一步" }))

    expect(await screen.findByText(/将挂到已有同名逻辑模型：1 个/)).toBeInTheDocument()
    await user.click(screen.getByRole("checkbox", { name: /挂关联/ }))
    expect(screen.getByText(/将挂到已有同名逻辑模型：0 个/)).toBeInTheDocument()
  })
})

describe("从供应商导入 · 熔断建议", () => {
  function assoc(over: Partial<{ ID: number; Backup: boolean | null; Status: boolean | null }>) {
    return {
      ID: 101, ModelID: 1, ProviderModel: "gpt-test", ProviderID: 11,
      ToolCall: true, StructuredOutput: false, Image: false, WithHeader: false,
      CustomerHeaders: {}, ExtraBody: null, Status: true, Weight: 1,
      InputPrice: 0, CacheReadPrice: 0, OutputPrice: 0, Currency: "CNY", Peak: null,
      ...over,
    }
  }

  it("主关联数 > MaxRetry 且未开熔断时，差异页给出开熔断建议", async () => {
    const user = userEvent.setup()
    // gpt-test: MaxRetry=3, Breaker=false（model() 基线），已有 3 条启用主关联；
    // 本次导入将再挂 1 条（dry_run 预查返回它）→ 4 > 3，触发建议
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    mocked.getModelProviders.mockResolvedValue([
      assoc({ ID: 101 }), assoc({ ID: 102 }), assoc({ ID: 103 }),
    ])
    mocked.batchCreateModelProviders.mockResolvedValue({
      dry_run: true,
      created: [{
        ID: 0, ModelID: 1, ProviderModel: "gpt-test", ProviderID: 11,
        ToolCall: false, StructuredOutput: false, Image: false, WithHeader: false,
        CustomerHeaders: {}, ExtraBody: null, Status: true, Weight: 1,
        InputPrice: 0, CacheReadPrice: 0, OutputPrice: 0, Currency: "CNY", Peak: null,
      }],
      skipped: [], failed: [], unfilled: [],
    })
    await openWizard(user)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("button", { name: "下一步" }))

    expect(await screen.findByText(/主关联数将超过重试次数限制/)).toBeInTheDocument()
    expect(screen.getByText(/gpt-test：主关联 4 条 > 重试 3 次/)).toBeInTheDocument()
    expect(screen.getByText(/备用关联永远不会接管/)).toBeInTheDocument()
  })

  it("主关联数未超限时不打扰；开了熔断也不打扰", async () => {
    const user = userEvent.setup()
    // 2 条已有 + 1 条将挂 = 3，不大于 MaxRetry=3
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    mocked.getModelProviders.mockResolvedValue([assoc({ ID: 101 }), assoc({ ID: 102 })])
    mocked.batchCreateModelProviders.mockResolvedValue({
      dry_run: true, created: [], skipped: [], failed: [], unfilled: [],
    })
    await openWizard(user)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("button", { name: "下一步" }))

    // 等差异页稳定后确认没有建议
    await screen.findByText(/将挂到已有同名逻辑模型：1 个/)
    await waitFor(() => expect(mocked.getModelProviders).toHaveBeenCalled())
    expect(screen.queryByText(/主关联数将超过重试次数限制/)).not.toBeInTheDocument()
  })
})

describe("从供应商导入 · 即时刷新", () => {
  it("导入落地后立即通知页面刷新，不等向导关闭", async () => {
    const user = userEvent.setup()
    const onCompleted = vi.fn()
    // gpt-test 已存在 → attach-only：第一步不发，第二步直接落关联
    mocked.getProviderModels.mockResolvedValue([pm("gpt-test")])
    mocked.batchCreateModelProviders.mockImplementation(async (req: unknown) => {
      const r = req as { dry_run?: boolean }
      if (r.dry_run) return { dry_run: true, created: [], skipped: [], failed: [], unfilled: [] }
      return {
        dry_run: false,
        created: [{ ID: 88, ModelID: 1, ProviderModel: "gpt-test", Weight: 1, Currency: "CNY" } as never],
        skipped: [], failed: [], unfilled: [],
      }
    })
    await openWizard(user, onCompleted)

    await user.click(await screen.findByRole("checkbox", { name: /gpt-test/ }))
    await user.click(screen.getByRole("button", { name: "下一步" }))
    await user.click(await screen.findByRole("button", { name: /导入 1 个/ }))

    expect(await screen.findByText(/新建 1 条/)).toBeInTheDocument()
    expect(onCompleted).toHaveBeenCalled()
  })
})
