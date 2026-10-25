#!/usr/bin/env python3
"""备用接管 × 池规模的语义验收车（方案 §五-1 的实测钉子）。

backup 的接管前提是"主池在本请求内被清空"。批量关联把单模型主关联数推到
N ≫ MaxRetry 之后，这个前提被静默破坏——本车把这件事用两台真进程钉在地面：

- **S1 熔断关（默认）**：20 条必死主关联 + 1 条活备用。每个请求最多删
  MaxRetry=3 条，池永不清空 → 备用**一次都不被碰**，每个请求都以
  "All retry failed" 收尾。
- **S2 熔断开**：同样 20+1。熔断按关联 ID 计数（每条攒满 5 次失败 Open），
  烧完约 ⌈20×5 / 3⌉ ≈ 34 个请求后主池在构造时即被清空 → 备用接管，
  客户端 200 且成功行 Backup=true。记录实际用了几个请求，就是"故障切换
  延迟随 N 线性放大"的实测数字。

用法：
    python e2e/backup_scale_check.py
"""

import os
import shutil
import subprocess
import sys
import time

E2E_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, E2E_DIR)
import run_matrix as rm  # noqa: E402

WORKDIR = os.path.join(rm.RUN_DIR, "backup-scale")
BIN = os.path.join(rm.RUN_DIR, "llmio-backup-scale.exe")

PRIMARY_N = 20


def hit_count(stub, name):
    return len([r for r in stub.received() if r["body"].get("model") == name])


def wait_rows(alias, min_count, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        rows = rm.api("GET", f"/logs?name={alias}&page=1&page_size=100")["data"] or []
        if len(rows) >= min_count:
            break
        time.sleep(0.2)
    time.sleep(0.5)
    return rm.api("GET", f"/logs?name={alias}&page=1&page_size=100")["data"] or []


def setup_model(alias, breaker):
    mid = rm.make_model(alias, prefer_direct=True, max_retry=3)
    rm.api("PUT", f"/models/{mid}", {
        "name": alias, "remark": "e2e", "max_retry": 3, "time_out": 60,
        "strategy": "lottery", "breaker": breaker,
    })
    return mid


def link_batch(mid, oc, pms, backup=False):
    body = {
        "model_ids": [mid], "provider_id": oc, "provider_models": pms,
        "defaults": {"weight": 1, "backup": backup,
                     "tool_call": True, "structured_output": True, "image": True},
    }
    return rm.api("POST", "/model-providers/batch", body)


def case_s1(stub, oc, text):
    """熔断关：N=20 死主 + 1 活备 → 备用零请求、每个请求 All retry failed"""
    alias = "bs-01"
    mid = setup_model(alias, breaker=False)
    pms = [f"{alias}-{i:02d}-boom" for i in range(PRIMARY_N)]
    link_batch(mid, oc, pms)
    # 活备用必须真的挂上：不挂的话"备用没接管"是空真——池里根本没有备用
    link_batch(mid, oc, [f"{alias}-text"], backup=True)

    status, raw = rm.chat("openai", rm.CLIENT_PATH["openai"],
                          dict(text, model=alias), rm.client_headers("openai"))
    rm.need(status != 200, "熔断关时这一发不该成功（备用接管前提被打破）")
    rm.need("All retry failed" in raw, f"应以重试耗尾：{raw[:200]}")
    # 多打几发，确认不是运气
    for _ in range(3):
        rm.chat("openai", rm.CLIENT_PATH["openai"],
                dict(text, model=alias), rm.client_headers("openai"))
    backups = sum(hit_count(stub, f"{alias}-{i:02d}-boom") for i in range(PRIMARY_N))
    rm.need(backups > 0, "死主关联确实被反复打过")
    backup_hits = hit_count(stub, f"{alias}-text")
    rm.need(backup_hits == 0,
            f"熔断关、N={PRIMARY_N}≫max_retry 时备用一次都不该被碰，实得 {backup_hits} 发——"
            f"这正是批量把 N 推大后 backup 接管前提被静默打破的形态")
    rm.need(True, f"S1 实测：{PRIMARY_N} 条死主关联、max_retry=3、熔断关——备用零请求（预期行为）")


def case_s2(stub, oc, text):
    """熔断开：同样 20+1 → 烧完熔断预算后备用接管，200 且 Backup=1"""
    alias = "bs-02"
    mid = setup_model(alias, breaker=True)
    pms = [f"{alias}-{i:02d}-boom" for i in range(PRIMARY_N)]
    link_batch(mid, oc, pms)
    link_batch(mid, oc, [f"{alias}-text"], backup=True)

    took = None
    for attempt in range(1, 61):
        status, raw = rm.chat("openai", rm.CLIENT_PATH["openai"],
                              dict(text, model=alias), rm.client_headers("openai"))
        if status == 200:
            took = attempt
            break
    rm.need(took is not None,
            f"熔断开后 60 个请求内备用应接管（20×5 次失败 ÷ 每请求 3 次 ≈ 34），实得最后一发 HTTP {status}：{raw[:200]}")

    rows = wait_rows(alias, 1)
    succ = [r for r in rows if r["Status"] == "success"]
    rm.need(len(succ) >= 1, "应至少一条成功行")
    rm.need(succ[0].get("ProviderModel") == f"{alias}-text",
            f"成功行应来自备用：{succ[0].get('ProviderModel')}")
    rm.need(succ[0].get("Backup") is True,
            f"成功行 Backup={succ[0].get('Backup')}，期望 true")
    print(f"   S2 实测：熔断开后第 {took} 个请求由备用接管"
          f"（≈{PRIMARY_N}×5 次失败 ÷ 3 次/请求 ≈ {PRIMARY_N * 5 // 3} 的量级）")


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
        oc = rm.make_provider("bscale-openai", "openai",
                              {"base_url": f"http://127.0.0.1:{rm.STUB_PORT}/openai", "api_key": "stub-key"})
        text = {"messages": [{"role": "user", "content": "你好"}], "max_tokens": 64}

        cases = [
            ("S1 熔断关·N≫max_retry 时备用不接管（既有语义的边界）", lambda: case_s1(stub, oc, text)),
            ("S2 熔断开·烧完预算后备用接管且日志标出", lambda: case_s2(stub, oc, text)),
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
