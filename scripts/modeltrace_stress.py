# -*- coding: utf-8 -*-
"""生成种子型随机基准：只存种子与期望概率，序列由 Go 按同一算法重建。

这样 Go 测试无需 Python 环境即可运行；本脚本只在需要更新基准时执行。

序列生成只使用整数运算，与 internal/modeltrace/stressprng.go 逐位一致。
"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modeltrace_probe as mt  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / "internal" / "modeltrace" / "testdata" / "stress.json"

MASK = (1 << 64) - 1


class SplitMix64:
    """与 Go 端 stressPRNG 完全一致的整数生成器。"""

    def __init__(self, seed):
        self.state = seed & MASK

    def next(self):
        self.state = (self.state + 0x9E3779B97F4A7C15) & MASK
        z = self.state
        z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & MASK
        z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & MASK
        return (z ^ (z >> 31)) & MASK

    def intn(self, n):
        if n == 0:
            return 0
        return self.next() % n


VALUE_MIN, VALUE_MAX = 1, 355


def clamp_value(v):
    return max(VALUE_MIN, min(VALUE_MAX, v))


def stress_sequence(name, seed, n):
    p = SplitMix64(seed)
    out = []
    if name == "uniform":
        for _ in range(n):
            out.append(1 + p.intn(355))
    elif name == "skewed-low":
        for _ in range(n):
            a, b = p.intn(355), p.intn(355)
            out.append(1 + min(a, b))
    elif name == "skewed-high":
        for _ in range(n):
            a, b = p.intn(355), p.intn(355)
            out.append(355 - min(a, b))
    elif name == "bimodal":
        for _ in range(n):
            if p.intn(2) == 0:
                out.append(1 + p.intn(40))
            else:
                out.append(300 + p.intn(56))
    elif name == "digits":
        last = [0, 0, 5]
        for _ in range(n):
            v = (1 + p.intn(35)) * 10 + last[p.intn(3)]
            out.append(clamp_value(v))
    elif name == "descending":
        for i in range(n):
            center = 300 if n <= 1 else 300 - 240 * i // (n - 1)
            out.append(clamp_value(center + p.intn(41) - 20))
    elif name == "boundary":
        choices = [1, 1, 355, 355, 2, 354]
        for _ in range(n):
            out.append(choices[p.intn(len(choices))])
    else:
        raise SystemExit("unknown generator: " + name)
    return out


GENERATORS = ["uniform", "skewed-low", "skewed-high", "bimodal", "digits", "descending", "boundary"]
LENGTHS = [80, 120, 218, 233, 247, 400]
BASE_SEED = 0x5EED_2026_1111


def main():
    bank = json.loads(mt.BANK_PATH.read_text(encoding="utf-8"))

    cases = []
    seed_counter = BASE_SEED
    for gi, name in enumerate(GENERATORS):
        for li, n in enumerate(LENGTHS):
            seed_counter += 1
            seeds = []
            answers = []
            for k in range(3):
                s = seed_counter + k * 1000003
                seeds.append(s)
                nums = stress_sequence(name, s, n)
                answers.append({"expected": n, "text": json.dumps(nums)})
            scored, used, beta = mt.analyze(answers, bank)
            cases.append({
                "name": f"{name}-{n}",
                "generator": name,
                "length": n,
                "seeds": seeds,
                "want_top": scored[0][0],
                "want_probs": {m: p for m, p, _ in scored[:6]},
            })

    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(cases, ensure_ascii=False, indent=1), encoding="utf-8")
    size = OUT.stat().st_size
    print(f"生成 {len(cases)} 个种子基准 -> {OUT} ({size} 字节)")
    tops = {}
    for c in cases:
        tops[c["want_top"]] = tops.get(c["want_top"], 0) + 1
    print("归因分布:", tops)


if __name__ == "__main__":
    main()
