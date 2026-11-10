# -*- coding: utf-8 -*-
"""对当前可调用的全部模型做 ModelTrace 归因，输出实际身份对照表。

同一账号、同一组挑战、同一指纹库，逐个模型跑三次挑战后归因。
"""
import json
import os
import sys
import time
from pathlib import Path

import requests

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modeltrace_probe as mt  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
BASE = "https://bps.codeai.de5.net/admin/api"
REPEATS = 3


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
    """跑三次挑战，返回 (outputs, 失败原因)。"""
    outputs = []
    for count, tmpl in zip(mt.COUNTS, mt.TEMPLATES):
        prompt = tmpl.format(count=count) + mt.DIRECT + \
            " Return one compact JSON array containing the complete sequence and no explanation."
        started = time.time()
        try:
            r = s.post(f"{BASE}/accounts/{acct_id}/test",
                       json={"model": model, "prompt": prompt, "effort": ""}, timeout=300)
        except Exception as e:
            return outputs, f"{type(e).__name__}: {e}"
        if r.status_code != 200:
            try:
                msg = r.json().get("error", {}).get("message", r.text[:120])
            except Exception:
                msg = r.text[:120]
            return outputs, f"HTTP {r.status_code}: {msg}"
        d = r.json()
        if not d.get("ok"):
            return outputs, f"ok=false terminal={d.get('terminal')}"
        outputs.append({"expected": count, "text": d.get("text") or "",
                        "reported": d.get("reported_model"),
                        "usage": d.get("usage") or {},
                        "elapsed": round(time.time() - started, 1)})
    return outputs, ""


def main():
    s = login()
    accounts = [a for a in s.get(f"{BASE}/accounts", timeout=30).json()["accounts"]
                if a["status"] == "ready"]
    if not accounts:
        raise SystemExit("没有可用账号")
    acct = accounts[0]

    # 模型列表取自持久化目录（即 /v1/models 的内容）
    catalog = s.get(f"{BASE}/models", timeout=30).json().get("catalog") or {}
    models = [m["id"] for m in catalog.get("data", [])]
    if not models:
        raise SystemExit("模型目录为空，请先同步")

    bank = json.loads(mt.BANK_PATH.read_text(encoding="utf-8"))
    print(f"账号: {acct['name']}   模型数: {len(models)}   指纹库: {len(bank['models'])} 模型\n", flush=True)

    rows = []
    for idx, model in enumerate(models, 1):
        print(f"[{idx}/{len(models)}] {model} …", flush=True)
        outputs, err = collect(s, acct["id"], model)
        if err:
            print(f"    失败: {err}", flush=True)
            rows.append({"model": model, "error": err})
            continue
        try:
            scored, used, beta = mt.analyze(outputs, bank)
        except SystemExit as e:
            print(f"    无法归因: {e}", flush=True)
            rows.append({"model": model, "error": str(e)})
            continue
        top_id, top_p, _ = scored[0]
        self_p = next((p for m, p, _ in scored if m == model), 0.0)
        runner = scored[1]
        rows.append({
            "model": model, "top": top_id, "top_p": top_p, "self_p": self_p,
            "runner": runner[0], "runner_p": runner[1],
            "reported": outputs[0].get("reported"), "used": used,
            "elapsed": round(sum(o.get("elapsed", 0) for o in outputs), 1),
            "detail": [(m, round(p, 6)) for m, p, _ in scored[:5]],
        })
        verdict = "一致" if top_id == model else f"实为 {top_id}"
        print(f"    -> {top_id} {top_p*100:.2f}%  (自判 {self_p*100:.2f}%)  {verdict}", flush=True)

    # 汇总表
    print("\n" + "=" * 92, flush=True)
    print(f"{'请求模型':<16}{'实际归因':<16}{'置信度':>9}{'自判概率':>10}{'次优候选':>22}", flush=True)
    print("-" * 92, flush=True)
    for r in rows:
        if r.get("error"):
            print(f"{r['model']:<16}{'失败':<16}{'':>9}{'':>10}  {r['error'][:30]}", flush=True)
            continue
        runner = f"{r['runner']} {r['runner_p']*100:.1f}%"
        print(f"{r['model']:<16}{r['top']:<16}{r['top_p']*100:>8.2f}%{r['self_p']*100:>9.2f}%{runner:>22}", flush=True)

    matched = [r for r in rows if not r.get("error") and r["top"] == r["model"]]
    print("-" * 92, flush=True)
    print(f"共 {len([r for r in rows if not r.get('error')])} 个模型完成归因，"
          f"{len(matched)} 个与请求名一致", flush=True)

    out = DATA / "modeltrace-all.json"
    out.write_text(json.dumps(rows, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n明细已保存: {out}", flush=True)


if __name__ == "__main__":
    main()
