# -*- coding: utf-8 -*-
"""并发稳定性测试：同一账号、同一模型，并发 N 路各打 M 次，记录每次真实结果与上游错误。

走管理台账号测试接口，链路与真实流量一致（桥接 → 上游 BPS）。
失败时完整保留上游返回的错误信息，不做截断。

用法:
    python scripts/concurrent-probe.py --account dongpan --model gpt-6.1-sol --concurrency 5 --times 10
"""
import argparse
import json
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
BASE = "http://127.0.0.1:8791/admin/api"

# 一道有确定答案的推理题：鸡兔同笼，35 头 94 脚 -> 鸡 23、兔 12。
PROMPT = (
    "笼子里有鸡和兔，共有 35 个头、94 只脚。"
    "请先逐步推理，然后在一行内给出最终答案，格式为：鸡=X,兔=Y"
)

_print_lock = threading.Lock()


def login():
    pw = (ROOT / "secrets" / "admin_password").read_text(encoding="utf-8").strip()
    s = requests.Session()
    r = s.post(f"{BASE}/login", json={"password": pw}, timeout=30)
    r.raise_for_status()
    s.headers.update({"X-CSRF-Token": r.json()["csrf_token"]})
    return s


def one_shot(session, acct_id, model, idx):
    """发一次请求，返回结构化结果。任何异常都转成可读的错误文本。"""
    started = time.time()
    out = {"idx": idx, "started": started, "ok": False,
           "http": None, "error": "", "text": "", "reported": "",
           "terminal": "", "reasoning_tokens": None, "output_tokens": None,
           "elapsed": 0.0}
    try:
        r = session.post(f"{BASE}/accounts/{acct_id}/test",
                         json={"model": model, "prompt": PROMPT, "effort": ""},
                         timeout=200)
        out["http"] = r.status_code
        out["elapsed"] = round(time.time() - started, 2)
        if r.status_code != 200:
            # 管理台把上游错误整体塞在 error.message 里，原样保留。
            try:
                doc = r.json()
                err = doc.get("error") or {}
                out["error"] = (err.get("code", "") + ": " + err.get("message", "")).strip(": ")
            except Exception:
                out["error"] = r.text
            return out
        d = r.json()
        out["ok"] = bool(d.get("ok"))
        out["text"] = d.get("text") or ""
        out["reported"] = d.get("reported_model") or ""
        out["terminal"] = d.get("terminal") or ""
        usage = d.get("usage") or {}
        out["output_tokens"] = usage.get("output_tokens")
        out["reasoning_tokens"] = (usage.get("output_tokens_details") or {}).get("reasoning_tokens")
        return out
    except Exception as e:
        out["elapsed"] = round(time.time() - started, 2)
        out["error"] = f"{type(e).__name__}: {e}"
        return out


def verdict_of(text):
    """从回答里抽出 鸡=X,兔=Y 判断对错。"""
    import re
    m = re.search(r"鸡\s*=\s*(\d+)\s*[,，]\s*兔\s*=\s*(\d+)", text or "")
    if not m:
        return "无最终答案格式"
    chicken, rabbit = int(m.group(1)), int(m.group(2))
    if chicken == 23 and rabbit == 12:
        return "正确"
    return f"错误(鸡={chicken},兔={rabbit})"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--account", default="dongpan")
    ap.add_argument("--model", default="gpt-6.1-sol")
    ap.add_argument("--concurrency", type=int, default=5)
    ap.add_argument("--times", type=int, default=10)
    args = ap.parse_args()

    session = login()
    accounts = {a["name"]: a["id"] for a in session.get(f"{BASE}/accounts", timeout=30).json()["accounts"]}
    if args.account not in accounts:
        raise SystemExit(f"账号不存在: {args.account}（可选: {list(accounts)}）")
    acct_id = accounts[args.account]

    print(f"账号: {args.account} ({acct_id})")
    print(f"模型: {args.model}")
    print(f"并发: {args.concurrency}  次数: {args.times}")
    print(f"提示词: {PROMPT[:40]}…\n")

    started = time.time()
    results = []
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(one_shot, session, acct_id, args.model, i + 1)
                   for i in range(args.times)]
        for fut in as_completed(futures):
            r = fut.result()
            results.append(r)
            with _print_lock:
                if r["ok"]:
                    v = verdict_of(r["text"])
                    print(f"[#{r['idx']:>2}] OK   {r['elapsed']:>6.1f}s  "
                          f"reported={r['reported']:<14} reasoning_tokens={r['reasoning_tokens']}  "
                          f"判定={v}")
                else:
                    print(f"[#{r['idx']:>2}] FAIL {r['elapsed']:>6.1f}s  HTTP={r['http']}  "
                          f"error={r['error'][:200]}")

    total_elapsed = round(time.time() - started, 1)
    results.sort(key=lambda x: x["idx"])

    ok = [r for r in results if r["ok"]]
    bad = [r for r in results if not r["ok"]]
    print("\n" + "=" * 96)
    print(f"总计 {len(results)} 次，成功 {len(ok)}，失败 {len(bad)}，墙钟耗时 {total_elapsed}s")
    if ok:
        lat = sorted(r["elapsed"] for r in ok)
        print(f"成功耗时: min={lat[0]}s median={lat[len(lat)//2]}s max={lat[-1]}s")
        verdicts = {}
        for r in ok:
            v = verdict_of(r["text"])
            verdicts[v] = verdicts.get(v, 0) + 1
        print("推理答案分布:", ", ".join(f"{k}×{v}" for k, v in verdicts.items()))
        reports = {}
        for r in ok:
            reports[r["reported"]] = reports.get(r["reported"], 0) + 1
        print("上游回报模型:", ", ".join(f"{k}×{v}" for k, v in reports.items()))

    if bad:
        print("\n" + "-" * 96)
        print("失败明细（上游原始错误）：")
        for r in bad:
            print(f"\n[#{r['idx']}] HTTP={r['http']} 耗时={r['elapsed']}s")
            print(f"  {r['error']}")

    out_file = DATA / f"concurrent-{args.model.replace('.', '_')}-{args.account.replace('@', '_')}.json"
    out_file.write_text(json.dumps({"account": args.account, "model": args.model,
                                    "concurrency": args.concurrency, "times": args.times,
                                    "wall_seconds": total_elapsed, "results": results},
                                   ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n完整结果已保存: {out_file}")
    return 0 if not bad else 1


if __name__ == "__main__":
    sys.exit(main())
