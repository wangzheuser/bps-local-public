# -*- coding: utf-8 -*-
"""直连上游并发测试，抓取 response.failed 事件的完整 error 对象。

管理台测试接口把上游错误简化成 "upstream reported error"，丢掉了 code/message。
这里绕过桥接直接打 BPS 端点，把每个失败请求的原始 error 结构完整打出来。

用法:
    python scripts/upstream-raw-probe.py --account 559e1802 --model gpt-6.1-sol --concurrency 5 --times 10
"""
import argparse
import base64
import json
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
UPSTREAM = "https://bps.openai.com/basispoints/api/responses"
PROXY = {"http": "http://127.0.0.1:7890", "https": "http://127.0.0.1:7890"}

PROMPT = (
    "笼子里有鸡和兔，共有 35 个头、94 只脚。"
    "请先逐步推理，然后在一行内给出最终答案，格式为：鸡=X,兔=Y"
)

_lock = threading.Lock()


def load_credential(fragment):
    for p in sorted((DATA / "accounts").glob("*.json")):
        if fragment and fragment not in p.name:
            continue
        try:
            doc = json.loads(p.read_text(encoding="utf-8"))
            if doc.get("access_token"):
                return p, doc
        except Exception:
            continue
    raise SystemExit("没有可用凭据")


def account_id_from_jwt(token):
    parts = token.split(".")
    if len(parts) < 2:
        return ""
    payload = parts[1] + "=" * (-len(parts[1]) % 4)
    try:
        claims = json.loads(base64.urlsafe_b64decode(payload))
    except Exception:
        return ""
    return (claims.get("https://api.openai.com/auth") or {}).get("chatgpt_account_id", "")


def headers(token, acct):
    return {
        "Authorization": "Bearer " + token,
        "Chatgpt-Account-Id": acct,
        "X-Openai-Account-Id": acct,
        "X-Basispoints-Auth-Mode": "chatgpt",
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
        "Origin": "https://bps.openai.com",
        "User-Agent": "Mozilla/5.0",
        "X-Openai-Internal-Basispoints-Client-Product": "basispoints-excel-plugin",
        "X-Openai-Internal-Basispoints-Client-Agent-Profile": "excel",
    }


def body_for(model, prompt, idx):
    return {
        "model": model,
        "model_selection": "explicit",
        "stream": True,
        "store": False,
        "input": [{"type": "message", "role": "user",
                   "content": [{"type": "input_text", "text": prompt}]}],
        "reasoning_effort": "low",
        "context_management": [{"type": "compaction", "compact_threshold": 200000}],
        "metadata": {"task_id": f"{idx:032d}", "turn_id": f"{idx:032d}", "agent_iteration": "1"},
    }


def one(token, acct, model, idx):
    started = time.time()
    out = {"idx": idx, "ok": False, "http": None, "elapsed": 0.0,
           "terminal": "", "text": "", "reported": "", "usage": None,
           "error_obj": None, "raw_terminal_event": "", "note": ""}
    try:
        r = requests.post(UPSTREAM, headers=headers(token, acct),
                          json=body_for(model, PROMPT, idx), stream=True,
                          timeout=200, proxies=PROXY)
        out["http"] = r.status_code
        if r.status_code != 200:
            out["elapsed"] = round(time.time() - started, 2)
            out["error_obj"] = {"http_status": r.status_code, "body": r.text[:3000]}
            return out
        text = ""
        for raw in r.iter_lines(decode_unicode=True):
            if not raw or not raw.startswith("data:"):
                continue
            payload = raw[5:].strip()
            if not payload or payload == "[DONE]":
                continue
            try:
                ev = json.loads(payload)
            except Exception:
                continue
            t = ev.get("type", "")
            if t == "response.output_text.delta":
                text += ev.get("delta", "")
            if t in ("response.completed", "response.incomplete",
                     "response.failed", "error"):
                out["terminal"] = t
                out["raw_terminal_event"] = json.dumps(ev, ensure_ascii=False)[:4000]
                resp = ev.get("response") or {}
                out["reported"] = resp.get("model") or ""
                out["usage"] = resp.get("usage")
                if t == "response.completed":
                    out["ok"] = True
                    out["text"] = text
                else:
                    # 保留完整错误结构：response.error / ev.error / 顶层字段
                    out["error_obj"] = {
                        "terminal": t,
                        "response_error": resp.get("error"),
                        "event_error": ev.get("error"),
                        "response_status": resp.get("status"),
                        "response_incomplete_details": resp.get("incomplete_details"),
                        "event_code": ev.get("code"),
                        "event_message": ev.get("message"),
                    }
        out["elapsed"] = round(time.time() - started, 2)
        if not out["terminal"]:
            out["note"] = "stream ended without terminal event"
        return out
    except Exception as e:
        out["elapsed"] = round(time.time() - started, 2)
        out["error_obj"] = {"exception": f"{type(e).__name__}: {e}"}
        return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--account", default="559e1802")
    ap.add_argument("--model", default="gpt-6.1-sol")
    ap.add_argument("--concurrency", type=int, default=5)
    ap.add_argument("--times", type=int, default=10)
    args = ap.parse_args()

    path, doc = load_credential(args.account)
    token = doc["access_token"]
    acct = account_id_from_jwt(token)
    print(f"凭据: {path.name}  账号: {acct[:8]}…")
    print(f"模型: {args.model}  并发: {args.concurrency}  次数: {args.times}\n")

    started = time.time()
    results = []
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(one, token, acct, args.model, i + 1)
                   for i in range(args.times)]
        for fut in as_completed(futures):
            r = fut.result()
            results.append(r)
            with _lock:
                if r["ok"]:
                    print(f"[#{r['idx']:>2}] OK   {r['elapsed']:>6.1f}s "
                          f"reported={r['reported']}")
                else:
                    err = r.get("error_obj") or {}
                    brief = json.dumps(err, ensure_ascii=False)[:220]
                    print(f"[#{r['idx']:>2}] FAIL {r['elapsed']:>6.1f}s HTTP={r['http']} {brief}")

    wall = round(time.time() - started, 1)
    results.sort(key=lambda x: x["idx"])
    ok = [r for r in results if r["ok"]]
    bad = [r for r in results if not r["ok"]]

    print("\n" + "=" * 96)
    print(f"总计 {len(results)} 次，成功 {len(ok)}，失败 {len(bad)}，墙钟 {wall}s")

    if bad:
        print("\n" + "-" * 96)
        print("失败请求的上游原始返回：")
        for r in bad:
            print(f"\n[#{r['idx']}] HTTP={r['http']} 耗时={r['elapsed']}s terminal={r['terminal'] or '(none)'}")
            print("  error_obj: " + json.dumps(r.get("error_obj"), ensure_ascii=False, indent=2).replace("\n", "\n  "))
            if r.get("raw_terminal_event"):
                print("  原始终止事件: " + r["raw_terminal_event"][:1500])

    out_file = DATA / f"upstream-raw-{args.model.replace('.', '_')}.json"
    out_file.write_text(json.dumps({"model": args.model, "concurrency": args.concurrency,
                                    "times": args.times, "wall_seconds": wall,
                                    "results": results}, ensure_ascii=False, indent=2),
                        encoding="utf-8")
    print(f"\n完整结果已保存: {out_file}")
    return 0 if not bad else 1


if __name__ == "__main__":
    sys.exit(main())
