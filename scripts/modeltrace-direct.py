# -*- coding: utf-8 -*-
"""直连 BPS 上游做 ModelTrace 归因，绕过桥接注入的 developer 消息。

对照目的：如果 gpt-5.6-sol 在"无开发者消息"的干净环境下仍被判为 luna，
则降智是上游计划限制；如果它自判成功，则说明是桥接环境干扰了指纹。

用法:
    python scripts/modeltrace-direct.py --model gpt-5.6-sol
    python scripts/modeltrace-direct.py --model gpt-5.6-sol --with-prologue  # 加回开发者消息做对照
"""
import argparse
import hashlib
import json
import sys
import time
from pathlib import Path

import requests

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modeltrace_probe as mt  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
UPSTREAM = "https://bps.openai.com/basispoints/api/responses"

# 桥接会注入的相同内容，用于对照组
BRIDGE_PROLOGUE = (
    "This request comes from an external Responses client. Return assistant text. "
    "Do not call Excel, Office, workbook or connector tools."
)


def load_credential():
    """取第一个账号的凭据。"""
    for p in sorted((DATA / "accounts").glob("*.json")):
        try:
            doc = json.loads(p.read_text(encoding="utf-8"))
            if doc.get("access_token"):
                return p, doc
        except Exception:
            continue
    raise SystemExit("没有可用凭据")


def account_id_from_jwt(token):
    import base64
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


def build_body(model, prompt, with_prologue):
    inp = []
    if with_prologue:
        inp.append({"type": "message", "role": "developer",
                    "content": [{"type": "input_text", "text": BRIDGE_PROLOGUE}]})
    inp.append({"type": "message", "role": "user",
                "content": [{"type": "input_text", "text": prompt}]})
    # 与桥接一致：上游要求 metadata 三个字段
    seed = f"{model}:{prompt[:64]}"
    task_id = hashlib.sha256(seed.encode()).hexdigest()[:32]
    turn_id = hashlib.sha256((seed + ":turn").encode()).hexdigest()[:32]
    return {
        "model": model,
        "model_selection": "explicit",
        "stream": True,
        "store": False,
        "input": inp,
        "reasoning_effort": "low",
        "context_management": [{"type": "compaction", "compact_threshold": 200000}],
        "metadata": {
            "task_id": task_id,
            "turn_id": turn_id,
            "agent_iteration": "1",
        },
    }


def call(token, acct, model, prompt, with_prologue):
    body = build_body(model, prompt, with_prologue)
    proxies = {"http": "http://127.0.0.1:7890", "https": "http://127.0.0.1:7890"}
    started = time.time()
    r = requests.post(UPSTREAM, headers=headers(token, acct), json=body,
                      stream=True, timeout=300, proxies=proxies)
    if r.status_code != 200:
        return None, None, f"HTTP {r.status_code}: {r.text[:200]}", 0
    text, reported, terminal = "", "", ""
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
        if t in ("response.completed", "response.failed", "error"):
            terminal = t
            resp = ev.get("response") or {}
            reported = resp.get("model", "")
            if t != "response.completed":
                err = resp.get("error") or ev.get("error") or {}
                return None, reported, f"{terminal}: {err.get('message','')[:120]}", time.time() - started
    return text, reported, terminal, time.time() - started


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="gpt-5.6-sol")
    ap.add_argument("--with-prologue", action="store_true",
                    help="加回桥接的 developer 消息做对照")
    args = ap.parse_args()

    path, doc = load_credential()
    token = doc["access_token"]
    acct = account_id_from_jwt(token)
    print(f"凭据: {path.name}  账号: {acct[:8]}…")
    print(f"模式: {'带 developer 消息（模拟桥接）' if args.with_prologue else '干净环境（直连无 prologue）'}")
    print(f"模型: {args.model}\n")

    outputs = []
    for i, (count, tmpl) in enumerate(zip(mt.COUNTS, mt.TEMPLATES), 1):
        prompt = tmpl.format(count=count) + mt.DIRECT + \
            " Return one compact JSON array containing the complete sequence and no explanation."
        print(f"[{i}/3] 请求 {count} 个数字 …", flush=True)
        text, reported, status, elapsed = call(token, acct, args.model, prompt, args.with_prologue)
        if text is None:
            print(f"    失败: {status}")
            continue
        nums = mt.parse_numbers(text)
        print(f"    完成 {elapsed:.1f}s reported={reported} terminal={status} "
              f"解析={len(nums)}/{count}")
        outputs.append({"expected": count, "text": text, "reported": reported})

    if not outputs:
        raise SystemExit("没有可用输出")

    bank = json.loads(mt.BANK_PATH.read_text(encoding="utf-8"))
    print("\n=== 输出解析 ===")
    scored, used, beta = mt.analyze(outputs, bank)

    print(f"\n=== 归因（{used} 份有效，β={beta:.2f}）===")
    for mid, p, s in scored[:6]:
        mark = "  ← 待测" if mid == args.model else ""
        print(f"  {mid:<20}{p*100:>8.2f}%{mark}")

    top = scored[0]
    self_p = next((p for m, p, _ in scored if m == args.model), 0.0)
    print(f"\n判定: {top[0]} ({top[1]*100:.2f}%)")
    print(f"自判概率: {self_p*100:.2f}%  ->  {'一致' if top[0] == args.model else '实为 ' + top[0]}")


if __name__ == "__main__":
    main()

