# -*- coding: utf-8 -*-
"""生成大规模随机基准，逐项比对 Python 与 Go 的归因结果。

覆盖：不同长度、不同分布、边界值、极端序列。
输出 internal/modeltrace/testdata/stress.json，由 Go 测试逐项校验。
"""
import json
import random
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modeltrace_probe as mt  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / "internal" / "modeltrace" / "testdata" / "stress.json"


def seq_uniform(rng, n):
    return [rng.randint(1, 355) for _ in range(n)]


def seq_skewed(rng, n):
    """偏向低值区，模拟某些模型的偏好。"""
    return [max(1, min(355, int(rng.gauss(60, 40)))) for _ in range(n)]


def seq_skewed_high(rng, n):
    return [max(1, min(355, int(rng.gauss(280, 50)))) for _ in range(n)]


def seq_bimodal(rng, n):
    return [rng.randint(1, 40) if rng.random() < 0.5 else rng.randint(300, 355) for _ in range(n)]


def seq_digits_biased(rng, n):
    """末位偏 0/5，检验有序块特征。"""
    out = []
    for _ in range(n):
        base = rng.randint(1, 35) * 10
        v = base + rng.choice([0, 0, 5])
        out.append(max(1, min(355, v)))
    return out


def seq_descending_trend(rng, n):
    """前半高值后半低值，检验分块特征。"""
    out = []
    for i in range(n):
        center = 300 - int(240 * i / max(1, n - 1))
        out.append(max(1, min(355, center + rng.randint(-20, 20))))
    return out


def seq_boundary(rng, n):
    """大量边界值 1 和 355。"""
    return [rng.choice([1, 1, 355, 355, 2, 354]) for _ in range(n)]


GENERATORS = [
    ("uniform", seq_uniform),
    ("skewed-low", seq_skewed),
    ("skewed-high", seq_skewed_high),
    ("bimodal", seq_bimodal),
    ("digits", seq_digits_biased),
    ("descending", seq_descending_trend),
    ("boundary", seq_boundary),
]

LENGTHS = [80, 120, 218, 233, 247, 400]


def as_text(numbers):
    return json.dumps(numbers)


def main():
    bank = json.loads(mt.BANK_PATH.read_text(encoding="utf-8"))
    rng = random.Random(20261111)

    cases = []
    for name, gen in GENERATORS:
        for n in LENGTHS:
            # 每个组合用 3 条答案，模拟真实探测
            answers = []
            for _ in range(3):
                nums = gen(rng, n)
                answers.append({"Expected": n, "Text": as_text(nums)})
            scored, used, beta = mt.analyze(
                [{"expected": a["Expected"], "text": a["Text"]} for a in answers], bank)
            cases.append({
                "name": f"{name}-{n}",
                "requested": scored[0][0],
                "used": used,
                "beta": beta,
                "answers": answers,
                "want_top": scored[0][0],
                "want_probs": {m: p for m, p, _ in scored[:6]},
                # 全量排行也记录下来，便于定位差异
                "want_all": [[m, p] for m, p, _ in scored],
            })

    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(cases, ensure_ascii=False, indent=1), encoding="utf-8")
    print(f"生成 {len(cases)} 个随机基准 -> {OUT}")
    tops = {}
    for c in cases:
        tops[c["want_top"]] = tops.get(c["want_top"], 0) + 1
    print("归因分布:", tops)


if __name__ == "__main__":
    main()

