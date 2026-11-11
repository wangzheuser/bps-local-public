# -*- coding: utf-8 -*-
"""账号 × 模型归因矩阵（增量、可续跑）。

每完成一组立即落盘，中断后可重跑续做；已测过的组合默认跳过。
环境变量：
    BPS_ONLY_ACCOUNT  只测指定账号
    BPS_FORCE=1       忽略已有结果，全部重测
"""
import json
import os
import sys
from pathlib import Path

import requests

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modeltrace_probe as mt  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
BASE = os.environ.get("BPS_ADMIN_BASE", "http://127.0.0.1:8791/admin/api")
OUT = DATA / "account-model-matrix.json"


def login():
    pw = os.environ.get("BPS_ADMIN_PASSWORD", "").strip()
    if not pw:
        pw = (ROOT / "secrets" / "admin_password").read_text(encoding="utf-8").strip()
    s = requests.Session()
    r = s.post(f"{BASE}/login", json={"password": pw}, timeout=30)
    r.raise_for_status()
    s.headers.update({"X-CSRF-Token": r.json()["csrf_token"]})
    return s


def collect(s, acct_id, model):
    outputs = []
    for count, tmpl in zip(mt.COUNTS, mt.TEMPLATES):
        prompt = tmpl.format(count=count) + mt.DIRECT + \
            " Return one compact JSON array containing the complete sequence and no explanation."
        try:
            r = s.post(f"{BASE}/accounts/{acct_id}/test",
                       json={"model": model, "prompt": prompt, "effort": ""}, timeout=300)
        except Exception as e:
            return outputs, f"{type(e).__name__}: {str(e)[:80]}"
        if r.status_code != 200:
            try:
                msg = r.json().get("error", {}).get("message", "")[:80]
            except Exception:
                msg = r.text[:80]
            return outputs, f"HTTP {r.status_code} {msg}"
        d = r.json()
        if not d.get("ok"):
            return outputs, f"not-ok {d.get('terminal')}"
        outputs.append({"expected": count, "text": d.get("text") or ""})
    return outputs, ""


def load_results():
    if not OUT.exists():
        return {}
    try:
        rows = json.loads(OUT.read_text(encoding="utf-8"))
    except Exception:
        return {}
    return {(r["account"], r["model"]): r for r in rows}


def save_results(results):
    rows = sorted(results.values(), key=lambda r: (r["account"], r["model"]))
    OUT.write_text(json.dumps(rows, ensure_ascii=False, indent=2), encoding="utf-8")


def main():
    models = sys.argv[1:] or ["gpt-5.6-sol"]
    s = login()
    accounts = [a for a in s.get(f"{BASE}/accounts", timeout=30).json()["accounts"]
                if a["status"] == "ready"]
    only = os.environ.get("BPS_ONLY_ACCOUNT", "").strip()
    if only:
        accounts = [a for a in accounts if a["name"] == only]
    if not accounts:
        raise SystemExit("没有匹配的账号")

    bank = json.loads(mt.BANK_PATH.read_text(encoding="utf-8"))
    results = {} if os.environ.get("BPS_FORCE") == "1" else load_results()

    for acct in accounts:
        for model in models:
            key = (acct["name"], model)
            if key in results and not results[key].get("error"):
                print(f"跳过 {acct['name']} / {model}（已测）", flush=True)
                continue
            print(f"[{acct['name']}] {model} …", flush=True)
            outputs, err = collect(s, acct["id"], model)
            if err:
                print(f"    失败: {err}", flush=True)
                results[key] = {"account": acct["name"], "model": model, "error": err}
                save_results(results)
                continue
            try:
                scored, used, _ = mt.analyze(outputs, bank)
            except SystemExit as e:
                print(f"    无法归因: {e}", flush=True)
                results[key] = {"account": acct["name"], "model": model, "error": str(e)}
                save_results(results)
                continue
            top, top_p, _ = scored[0]
            self_p = next((p for m, p, _ in scored if m == model), 0.0)
            results[key] = {
                "account": acct["name"], "model": model,
                "top": top, "top_p": top_p, "self_p": self_p, "used": used,
                "detail": [[m, round(p, 6)] for m, p, _ in scored[:4]],
            }
            save_results(results)   # 立即落盘，中断不丢
            print(f"    -> {top} {top_p*100:.2f}%  (自判 {self_p*100:.2f}%)", flush=True)

    # 矩阵
    print("\n" + "=" * 96, flush=True)
    head = f"{'账号':<22}"
    for m in models:
        head += f"{m.replace('gpt-', ''):>11}"
    print(head, flush=True)
    print("-" * 96, flush=True)
    for acct in accounts:
        row = f"{acct['name']:<22}"
        for m in models:
            r = results.get((acct["name"], m), {})
            if r.get("error"):
                row += f"{'ERR':>11}"
            else:
                row += f"{'%.0f%%' % (r['self_p']*100) + ('✓' if r['top'] == m else '✗'):>11}"
        print(row, flush=True)
    print("-" * 96, flush=True)
    print("✓=真实提供  ✗=被判为其他模型", flush=True)


if __name__ == "__main__":
    main()
