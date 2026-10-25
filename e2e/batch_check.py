#!/usr/bin/env python3
"""批量关联端点的验收车。

复用 run_matrix 的机器（桩上游、独立二进制、管理 API 建档），用例：

- **C1 dry_run 零写入**：预览返回完整行且 ID=0，库里一行不多。
- **C2 批量创建 + 三态语义**：显式 0 价原样保留（币种兜底 CNY）、空位物化
  成 false/0；建出来的关联当场接一发真实流量（桩 200）。
- **C3 skip_existing 幂等重交**：整批重交全 skipped 且带 existing_status，
  库里不因重交多出一行（唯一索引兜底）。
- **C4 批量启停/删除幂等**：停用后模型无可用关联、请求失败；批量删除后
  重交删除，not_found 不算失败。
- **C5 入参卫生**：批内重复、weight 0、不存在的模型——整批 400，不落一行。

自动填写策略在开场就关掉：e2e 环境对 models.dev 的网络可达性不可控，
开着会把"补了什么"变成取决于外网的随机结果；补值逻辑由 Go 单测
（stub 数据源）钉死，这里只测批量链路本身。

用法：
    python e2e/batch_check.py
"""

import json
import os
import shutil
import subprocess
import sys

E2E_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, E2E_DIR)
import run_matrix as rm  # noqa: E402

WORKDIR = os.path.join(rm.RUN_DIR, "batch")
BIN = os.path.join(rm.RUN_DIR, "llmio-batch.exe")


def batch(body):
    return rm.api("POST", "/model-providers/batch", body)


def rows_of(model_id):
    return rm.api("GET", f"/model-providers?model_id={model_id}") or []


def case_c1(oc):
    mid = rm.make_model("bc-1", prefer_direct=True, max_retry=2)
    resp = batch({
        "model_ids": [mid], "provider_id": oc,
        "provider_models": ["bc-1-a", "bc-1-b"],
        "defaults": {"weight": 1}, "dry_run": True,
    })
    rm.need(resp["dry_run"] is True, "dry_run 应回显 true")
    rm.need(len(resp["created"]) == 2, f"预览应 2 行，实得 {len(resp['created'])}")
    rm.need(all(row["ID"] == 0 for row in resp["created"]),
            f"预览行 ID 应全为 0（未落库），实得 {[row['ID'] for row in resp['created']]}")
    rm.need(len(rows_of(mid)) == 0, "dry_run 后库里不该有任何一行")


def case_c2(oc, text):
    mid = rm.make_model("bc-2", prefer_direct=True, max_retry=2)
    resp = batch({
        "model_ids": [mid], "provider_id": oc,
        "provider_models": ["bc-2-a", "bc-2-b"],
        # input_price 显式 0（免费上游的表达）；tool_call 留空（交给补全，
        # 策略已关 → 物化 false）；币种不传（兜底 CNY）
        "defaults": {"weight": 2, "input_price": 0},
    })
    rm.need(len(resp["created"]) == 2, f"应建 2 行，实得 {resp['created']}")
    for row in resp["created"]:
        rm.need(row["InputPrice"] == 0, f"显式 0 价不能被改写：{row['InputPrice']}")
        rm.need(row["Currency"] == "CNY", f"币种应兜底 CNY，实得 {row['Currency']}")
        rm.need(row["ToolCall"] is False, f"空位物化 false，实得 {row['ToolCall']}")
        rm.need(row["Weight"] == 2, f"weight 应跟 defaults，实得 {row['Weight']}")
    rm.need(len(resp["unfilled"]) == 0, f"策略关着不该有 unfilled：{resp['unfilled']}")

    # 建出来的关联当场接一发：批量建的行不是摆设
    status, raw = rm.chat("openai", rm.CLIENT_PATH["openai"],
                          dict(text, model="bc-2"), rm.client_headers("openai"))
    rm.need(status == 200, f"批量建的关联应可服务请求，实得 HTTP {status}：{raw[:300]}")


def case_c3(oc):
    mid = rm.make_model("bc-3", prefer_direct=True, max_retry=2)
    body = {
        "model_ids": [mid], "provider_id": oc,
        "provider_models": ["bc-3-a"], "defaults": {"weight": 1},
    }
    first = batch(body)
    rm.need(len(first["created"]) == 1, "首次应建 1 行")
    again = batch(body)
    rm.need(len(again["created"]) == 0, f"重交不该再建：{again['created']}")
    rm.need(len(again["skipped"]) == 1, "重交应报 1 条 skipped")
    skip = again["skipped"][0]
    rm.need(skip["reason"] == "exists", f"skipped 原因应为 exists，实得 {skip['reason']}")
    rm.need(skip["existing_status"] is True, f"应回显 existing_status=true，实得 {skip['existing_status']}")
    rm.need(len(rows_of(mid)) == 1, "重交后库里仍应只有 1 行（幂等）")


def case_c4(oc, text):
    mid = rm.make_model("bc-4", prefer_direct=True, max_retry=2)
    created = batch({
        "model_ids": [mid], "provider_id": oc,
        "provider_models": ["bc-4-a", "bc-4-b"], "defaults": {"weight": 1},
    })["created"]
    ids = [row["ID"] for row in created]

    rm.api("POST", "/model-providers/batch/status", {"ids": ids, "status": False})
    status, _ = rm.chat("openai", rm.CLIENT_PATH["openai"],
                        dict(text, model="bc-4"), rm.client_headers("openai"))
    rm.need(status != 200, "全量停用后该模型不应再有可用关联")

    resp = rm.api("POST", "/model-providers/batch/delete", {"ids": ids + [424242]})
    rm.need(sorted(resp["deleted"]) == sorted(ids), f"应删 {ids}，实得 {resp}")
    rm.need(resp["not_found"] == [424242], f"不存在的 id 进 not_found：{resp}")
    rm.need(len(rows_of(mid)) == 0, "删完应查不到行")

    again = rm.api("POST", "/model-providers/batch/delete", {"ids": ids})
    rm.need(again["deleted"] == [] and again["not_found"] == ids,
            f"重交删除应全 not_found：{again}")


def case_c5(oc):
    mid = rm.make_model("bc-5", prefer_direct=True, max_retry=2)
    before = len(rows_of(mid))
    bad_bodies = [
        ("provider_models 批内重复", {
            "model_ids": [mid], "provider_id": oc,
            "provider_models": ["bc-5-a", "bc-5-a"], "defaults": {"weight": 1}}),
        ("weight 为 0", {
            "model_ids": [mid], "provider_id": oc,
            "provider_models": ["bc-5-a"], "defaults": {"weight": 0}}),
        ("model_ids 含不存在的 id", {
            "model_ids": [mid, 424242], "provider_id": oc,
            "provider_models": ["bc-5-a"], "defaults": {"weight": 1}}),
        ("provider 不存在", {
            "model_ids": [mid], "provider_id": 424242,
            "provider_models": ["bc-5-a"], "defaults": {"weight": 1}}),
    ]
    for label, body in bad_bodies:
        try:
            batch(body)
            raise rm.Failure(f"{label} 应整批 400，实际成功了")
        except RuntimeError as err:
            rm.need("400" in str(err), f"{label} 应 400，实得 {str(err)[:200]}")
    rm.need(len(rows_of(mid)) == before, "整批 400 后库里不该多任何一行")


def main():
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    shutil.rmtree(WORKDIR, ignore_errors=True)
    os.makedirs(WORKDIR, exist_ok=True)

    print("→ 构建二进制")
    rm.BIN = BIN
    subprocess.run(["go", "build", "-o", BIN, "."], cwd=rm.REPO, check=True)

    stub = rm.Stub(WORKDIR)
    stub.start()
    llmio = rm.Llmio(WORKDIR)
    results = []
    try:
        llmio.start()
        print(f"→ llmio {rm.BASE}（工作目录 {WORKDIR}）")
        # 关自动填写：批量补值的行为不取决于这台机器能不能上 models.dev
        rm.api("PUT", "/config/model_autofill_policy",
               {"value": json.dumps({"enabled": False})})

        oc = rm.make_provider("batch-openai", "openai",
                              {"base_url": f"http://127.0.0.1:{rm.STUB_PORT}/openai", "api_key": "stub-key"})
        text = {"messages": [{"role": "user", "content": "你好"}], "max_tokens": 64}

        cases = [
            ("C1 dry_run 零写入·预览行 ID=0", lambda: case_c1(oc)),
            ("C2 批量创建·三态与币种兜底·当场接流量", lambda: case_c2(oc, text)),
            ("C3 skip_existing 幂等重交·回显启用状态", lambda: case_c3(oc)),
            ("C4 批量启停/删除幂等·not_found 不算失败", lambda: case_c4(oc, text)),
            ("C5 入参卫生整批 400", lambda: case_c5(oc)),
        ]
        for label, fn in cases:
            try:
                fn()
                rm.record(results, label, "ok")
            except rm.Failure as e:
                rm.record(results, label, "fail", str(e))
            except Exception as e:  # noqa: BLE001
                rm.record(results, label, "fail", f"{type(e).__name__}: {e}")
    finally:
        llmio.stop()
        stub.stop()

    ok = sum(1 for _, v, _ in results if v == "ok")
    return 0 if ok == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
