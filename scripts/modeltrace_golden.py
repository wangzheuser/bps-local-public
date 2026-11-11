# -*- coding: utf-8 -*-
"""生成 Go 端回归基准（golden）——用真实采集的回答跑 Python 参考实现。

用法:
    python scripts/modeltrace_golden.py            # 用已有实测记录
    python scripts/modeltrace_golden.py --live     # 现采一次再生成
"""
import argparse
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modeltrace_probe as mt  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
OUT = ROOT / "internal" / "modeltrace" / "testdata" / "golden.json"


def synthetic_answers(seed: int, count: int) -> str:
    """生成确定性数字序列，用于无网络时构造基准。

    用固定 LCG 生成，保证可复现；分布刻意偏离均匀以产生可归因的信号。
    """
    state = seed
    out = []
    for _ in range(count):
        state = (1103515245 * state + 12345) % (2 ** 31)
        # 偏向中段并叠加模 10 偏好，模拟真实模型的取值习惯
        v = 1 + (state % 355)
        if v > 300:
            v = 1 + (state % 200)
        out.append(v)
    return json.dumps(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--live", action="store_true", help="实际请求模型采集回答")
    args = ap.parse_args()

    bank = json.loads(mt.BANK_PATH.read_text(encoding="utf-8"))

    cases = []
    if args.live:
        import requests
        base = os.environ.get("BPS_ADMIN_BASE", "http://127.0.0.1:8791/admin/api")
        pw = os.environ.get("BPS_ADMIN_PASSWORD", "").strip()
        if not pw:
            pw = (ROOT / "secrets" / "admin_password").read_text(encoding="utf-8").strip()
        s = requests.Session()
        r = s.post(f"{base}/login", json={"password": pw}, timeout=30)
        s.headers.update({"X-CSRF-Token": r.json()["csrf_token"]})
        accts = {a["name"]: a["id"] for a in s.get(f"{base}/accounts", timeout=30).json()["accounts"]}
        name = os.environ.get("BPS_ONLY_ACCOUNT", "wangzijie")
        acct = accts.get(name)
        if not acct:
            raise SystemExit(f"账号不存在: {name}")
        for model in ("gpt-5.6-luna", "gpt-5.6-sol"):
            answers = []
            for count, tmpl in zip(mt.COUNTS, mt.TEMPLATES):
                prompt = tmpl.format(count=count) + mt.DIRECT + \
                    " Return one compact JSON array containing the complete sequence and no explanation."
                r = s.post(f"{base}/accounts/{acct}/test",
                           json={"model": model, "prompt": prompt, "effort": ""}, timeout=300)
                d = r.json()
                answers.append({"Expected": count, "Text": d.get("text") or ""})
            scored, used, _ = mt.analyze(
                [{"expected": a["Expected"], "text": a["Text"]} for a in answers], bank)
            cases.append({
                "name": f"live-{model}",
                "requested": model,
                "answers": answers,
                "want_top": scored[0][0],
                "want_probs": {m: round(p, 8) for m, p, _ in scored[:4]},
            })
    else:
        # 固定种子的合成序列：不依赖网络，仅校验移植一致性
        for seed, name in ((12345, "synthetic-a"), (777, "synthetic-b"), (2024, "synthetic-c")):
            answers = []
            for count in mt.COUNTS:
                answers.append({"Expected": count, "Text": synthetic_answers(seed, count)})
            scored, used, _ = mt.analyze(
                [{"expected": a["Expected"], "text": a["Text"]} for a in answers], bank)
            cases.append({
                "name": name,
                "requested": scored[0][0],
                "answers": answers,
                "want_top": scored[0][0],
                "want_probs": {m: round(p, 8) for m, p, _ in scored[:4]},
            })

    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(cases, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"写出 {len(cases)} 个基准用例 -> {OUT}")
    for c in cases:
        print(f"  {c['name']}: top={c['want_top']} p={c['want_probs']}")


if __name__ == "__main__":
    main()
