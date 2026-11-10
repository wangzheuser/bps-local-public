# -*- coding: utf-8 -*-
"""逐模型重复测试，检查「请求模型」与「上游实际返回模型」的一致性。

对每个模型调用 5 次管理端账号测试接口，记录返回模型、耗时、输出与用量，
最后给出每个模型的一致性统计与全局汇总。
"""
import json
import sys
import time
from collections import Counter
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
ADMIN = "http://127.0.0.1:8791/admin/api"
PROMPT = "hi"
REPEATS = 5


def login():
    pw = (ROOT / "secrets" / "admin_password").read_text(encoding="utf-8").strip()
    s = requests.Session()
    r = s.post(f"{ADMIN}/login", json={"password": pw}, timeout=20)
    r.raise_for_status()
    csrf = r.json()["csrf_token"]
    s.headers.update({"X-CSRF-Token": csrf})
    return s


def account_id(s):
    r = s.get(f"{ADMIN}/accounts", timeout=20)
    r.raise_for_status()
    accounts = r.json()["accounts"]
    if not accounts:
        raise SystemExit("no account configured")
    return accounts[0]["id"], accounts[0]["name"]


def models(s, acct):
    r = s.get(f"{ADMIN}/accounts/{acct}/models", timeout=40)
    r.raise_for_status()
    return [m["id"] for m in r.json()["models"]]


def run_test(s, acct, model, timeout=180):
    started = time.time()
    try:
        r = s.post(f"{ADMIN}/accounts/{acct}/test",
                   json={"model": model, "prompt": PROMPT, "effort": ""},
                   timeout=timeout)
        elapsed = round(time.time() - started, 1)
        if r.status_code != 200:
            try:
                msg = r.json().get("error", {}).get("message", r.text[:160])
            except Exception:
                msg = r.text[:160]
            return {"ok": False, "error": f"HTTP {r.status_code}: {msg}", "elapsed": elapsed}
        d = r.json()
        return {
            "ok": bool(d.get("ok")),
            "requested": d.get("requested_model", ""),
            "reported": d.get("reported_model", ""),
            "text": (d.get("text") or "").strip(),
            "latency_ms": d.get("latency_ms", 0),
            "usage": d.get("usage") or {},
            "elapsed": elapsed,
            "error": "",
        }
    except Exception as e:
        return {"ok": False, "error": f"{type(e).__name__}: {str(e)[:140]}",
                "elapsed": round(time.time() - started, 1)}


def main():
    s = login()
    acct, name = account_id(s)
    model_ids = models(s, acct)
    print(f"account: {name}  models: {len(model_ids)}  repeats: {REPEATS}\n")

    all_rows = []
    for m in model_ids:
        print(f"=== {m} ===")
        reported = Counter()
        ok = 0
        for i in range(1, REPEATS + 1):
            r = run_test(s, acct, m)
            r["model"] = m
            r["run"] = i
            all_rows.append(r)
            if r["ok"]:
                ok += 1
                reported[r["reported"]] += 1
                u = r["usage"]
                print(f"  #{i} ok  {r['latency_ms']:>6}ms  reported={r['reported']:<16} "
                      f"in={u.get('input_tokens')} out={u.get('output_tokens')} "
                      f"cached={(u.get('input_tokens_details') or {}).get('cached_tokens')}")
            else:
                print(f"  #{i} FAIL {r['elapsed']:>6}s  {r['error'][:120]}")
        same = reported.get(m, 0)
        print(f"  -> success {ok}/{REPEATS}, exact match {same}/{ok if ok else 0}, "
              f"distribution {dict(reported)}\n")

    # 汇总
    print("=" * 78)
    print(f"{'model':<16}{'ok':<8}{'exact':<8}{'reported distribution'}")
    print("-" * 78)
    total_ok = total_exact = 0
    for m in model_ids:
        rows = [r for r in all_rows if r["model"] == m]
        okrows = [r for r in rows if r["ok"]]
        dist = Counter(r["reported"] for r in okrows)
        exact = dist.get(m, 0)
        total_ok += len(okrows)
        total_exact += exact
        dist_str = ", ".join(f"{k}×{v}" for k, v in dist.items()) or "-"
        print(f"{m:<16}{len(okrows)}/{REPEATS:<6}{exact}/{len(okrows) if okrows else 0:<6}{dist_str}")

    print("-" * 78)
    print(f"TOTAL: {total_ok}/{len(all_rows)} succeeded, "
          f"{total_exact}/{total_ok if total_ok else 0} exact model match")

    lat = [r["latency_ms"] for r in all_rows if r["ok"]]
    if lat:
        lat.sort()
        print(f"latency: min={lat[0]}ms p50={lat[len(lat)//2]}ms max={lat[-1]}ms")

    (DATA / "model-consistency.json").write_text(
        json.dumps(all_rows, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\nraw results: {DATA / 'model-consistency.json'}")
    sys.exit(0)


if __name__ == "__main__":
    main()
