# -*- coding: utf-8 -*-
"""用 ModelTrace 挑战测试 gpt-5.6-sol 的真实身份。

流程：
1. 取 environment-01 的三条挑战（与建库同源，counts 218/233/247）
2. 经 bps-server 管理端账号测试接口发给 gpt-5.6-sol
3. 解析数字序列，校验是否达到最低数量
4. 用官方 unified_bank.json 做归因

用法：
    python scripts/modeltrace-probe.py [--model gpt-5.6-sol] [--account NAME]
"""
import argparse
import os
import json
import re
import sys
import time
from pathlib import Path

import numpy as np
import requests

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
BANK_PATH = DATA / "unified_bank.json"
BASE = os.environ.get("BPS_ADMIN_BASE", "http://127.0.0.1:8791/admin/api")

# --- 与 challenge_suite.py 保持一致的 environment-01 ---
DIRECT = (
    " The current language model must complete this directly without tools, Python, code execution, calculators, search, APIs, "
    "or external random generators. Choose every position separately. Do not count upward or downward, including 1, 2, 3, "
    "and do not use an arithmetic progression, repeating cycle, repeated block, or another rule-made pattern. "
    "Accidental repetitions are valid. Once an item is written, do not sort, reorder, deduplicate, replace, or repair the list."
)
TEMPLATES = (
    "For each of {count} positions, make one separate first-instinct choice of an integer from 1 to 355 inclusive.",
    "Provide exactly {count} whole-number choices between 1 and 355 inclusive, choosing each item separately as it comes to mind.",
    "Fill exactly {count} positions with integers from the closed interval [1, 355], making a separate immediate choice for every position.",
)
COUNTS = (218, 233, 247)

VALUE_MIN, VALUE_MAX = 1, 355
ALPHA = 0.5


def parse_numbers(text):
    """与 fingerprint.py 的 parse_numbers 一致：最长数字段，字母分隔切段。"""
    runs, cur, prev = [], [], 0
    for m in re.finditer(r"\d+", text):
        sep = text[prev:m.start()]
        v = int(m.group())
        if cur and any(c.isalpha() for c in sep):
            runs.append(cur)
            cur = []
        if VALUE_MIN <= v <= VALUE_MAX:
            cur.append(v)
        prev = m.end()
    if cur:
        runs.append(cur)
    return max(runs, key=len) if runs else []


def count_numbers(nums):
    counts = [0] * (VALUE_MAX - VALUE_MIN + 1)
    for n in nums:
        counts[n - VALUE_MIN] += 1
    return counts


def standardize(vals):
    a = np.asarray(vals, dtype=np.float64)
    scale = max(float(a.std()), 1e-12)
    return (a - a.mean()) / scale


def hellinger_feature(counts):
    v = np.asarray(counts, dtype=np.float64) + ALPHA
    return np.sqrt(v / v.sum())


def ordered_block_feature(nums):
    vals = np.asarray(nums, dtype=np.float64)
    pieces = []
    for chunk in np.array_split(vals, 4):
        c, _ = np.histogram(chunk, bins=16, range=(1.0, 356.0))
        s = c.astype(np.float64) + 0.5
        pieces.append(np.sqrt(s / s.sum()))
    last = np.bincount(vals.astype(int) % 10, minlength=10).astype(np.float64) + 0.5
    pieces.append(np.sqrt(last / last.sum()))
    return np.concatenate(pieces)


def robust_score_counts(counts, bank):
    r = bank["robust"]
    h = r["hellinger"]
    feat = hellinger_feature(counts)
    proj = (feat - np.asarray(h["feature_mean"])) / np.asarray(h["feature_scale"])
    basis = np.asarray(h["nuisance_basis"])
    if basis.size:
        proj -= (proj @ basis.T) @ basis
    proj /= max(float(np.linalg.norm(proj)), 1e-12)
    nuis = proj @ np.asarray(h["centroids"]).T
    return standardize(nuis).tolist()


def ordered_block_scores(nums, bank):
    a = bank["robust"]["ordered_blocks"]
    feat = ordered_block_feature(nums)
    std = (feat - np.asarray(a["feature_mean"])) / np.asarray(a["feature_scale"])
    norm = std / max(float(np.linalg.norm(std)), 1e-12)
    templates = np.asarray(a["environment_centroids"])
    env = np.stack([norm @ c.T for c in templates])
    tmpl = standardize(np.max(env, axis=0))
    proj = std.copy()
    basis = np.asarray(a["nuisance_basis"])
    if basis.size:
        proj -= (proj @ basis.T) @ basis
    proj /= max(float(np.linalg.norm(proj)), 1e-12)
    nuis = standardize(proj @ np.asarray(a["centroids"]).T)
    return standardize(0.5 * tmpl + 0.5 * nuis).tolist()


def robust_score_numbers(nums, bank):
    marginal = robust_score_counts(count_numbers(nums), bank)
    art = bank["robust"].get("ordered_blocks")
    w = float(art.get("weight", 0.0)) if art else 0.0
    if not art or w == 0.0:
        return marginal
    ordered = ordered_block_scores(nums, bank)
    return [(1 - w) * m + w * o for m, o in zip(marginal, ordered)]


def softmax(vals):
    m = max(vals)
    e = np.exp(np.asarray(vals) - m)
    return (e / e.sum()).tolist()


def analyze(outputs, bank):
    model_ids = [m["id"] for m in bank["models"]]
    valid = []
    for item in outputs:
        nums = parse_numbers(item["text"])
        minimum = max(80, int(np.ceil(item["expected"] * 0.55))) if item["expected"] else 80
        ok = len(nums) >= minimum
        print(f"  {item['expected']:>4} 期望 -> 解析 {len(nums):>4} 个，最低要求 {minimum:>3} -> "
              f"{'采用' if ok else '丢弃'}")
        if ok:
            valid.append(robust_score_numbers(nums, bank))
    if not valid:
        raise SystemExit("没有可用回答")

    combined = [sum(v[i] for v in valid) / len(valid) for i in range(len(model_ids))]
    beta = float(bank["calibration"][str(min(len(valid), 3))]["beta"])
    probs = softmax([beta * s for s in combined])
    rows = sorted(zip(model_ids, probs, combined), key=lambda x: -x[1])
    return rows, len(valid), beta


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="gpt-5.6-sol")
    ap.add_argument("--account", default=os.environ.get("BPS_ACCOUNT_NAME", ""),
                    help="账号名称；留空则使用第一个可用账号（或设 BPS_ACCOUNT_NAME）")
    ap.add_argument("--reuse", action="store_true", help="复用已保存的输出，不重新请求")
    args = ap.parse_args()

    out_file = DATA / f"modeltrace-{args.model.replace('.', '_')}.json"

    if args.reuse and out_file.exists():
        outputs = json.loads(out_file.read_text(encoding="utf-8"))
        print(f"复用 {out_file}")
    else:
        # 服务器密码优先取环境变量，避免把远端密钥落到本地文件
        pw = os.environ.get("BPS_ADMIN_PASSWORD", "").strip()
        if not pw:
            pw = (ROOT / "secrets" / "admin_password").read_text(encoding="utf-8").strip()
        s = requests.Session()
        r = s.post(f"{BASE}/login", json={"password": pw}, timeout=30)
        r.raise_for_status()
        s.headers.update({"X-CSRF-Token": r.json()["csrf_token"]})
        accts = {a["name"]: a["id"] for a in s.get(f"{BASE}/accounts", timeout=30).json()["accounts"]}
        if not args.account:
            if not accts:
                raise SystemExit("没有可用账号，请先授权一个账号")
            args.account = next(iter(accts))
        if args.account not in accts:
            raise SystemExit(f"账号不存在: {args.account}（可选: {list(accts)}）")
        acct = accts[args.account]

        outputs = []
        for i, (count, tmpl) in enumerate(zip(COUNTS, TEMPLATES), 1):
            prompt = tmpl.format(count=count) + DIRECT + \
                " Return one compact JSON array containing the complete sequence and no explanation."
            print(f"[{i}/3] 请求 {args.model}，期望 {count} 个数字 …", flush=True)
            started = time.time()
            r = s.post(f"{BASE}/accounts/{acct}/test",
                       json={"model": args.model, "prompt": prompt, "effort": ""}, timeout=300)
            if r.status_code != 200:
                print(f"  HTTP {r.status_code}: {r.text[:200]}")
                continue
            d = r.json()
            usage = d.get("usage") or {}
            print(f"  完成 {time.time()-started:.1f}s reported={d.get('reported_model')} "
                  f"out_tokens={usage.get('output_tokens')} terminal={d.get('terminal')}")
            outputs.append({"expected": count, "text": d.get("text") or "",
                            "reported": d.get("reported_model"), "usage": usage})
        out_file.write_text(json.dumps(outputs, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"已保存 {out_file}")

    bank = json.loads(BANK_PATH.read_text(encoding="utf-8"))
    print(f"\n指纹库: {len(bank['models'])} 个模型，构建于 {bank['built_at']}")
    print(f"待测: {args.model}\n")

    print("=== 输出解析 ===")
    rows, used, beta = analyze(outputs, bank)

    print(f"\n=== 归因结果（{used} 份有效回答，β={beta:.3f}）===")
    print(f"{'模型':<28}{'概率':>9}{'得分':>9}")
    for mid, p, s in rows[:8]:
        mark = "  ← 待测" if mid == args.model else ""
        print(f"{mid:<28}{p*100:>8.2f}%{s:>9.3f}{mark}")

    top = rows[0]
    self_prob = next(p for m, p, _ in rows if m == args.model)
    print(f"\n最高概率: {top[0]} ({top[1]*100:.2f}%)")
    print(f"待测模型自身概率: {self_prob*100:.2f}%")
    print(f"判定: {'一致' if top[0] == args.model else '不一致 —— 更像 ' + top[0]}")


if __name__ == "__main__":
    main()


