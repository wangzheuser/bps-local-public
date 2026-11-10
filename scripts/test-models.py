# -*- coding: utf-8 -*-
"""用现有 API Key 逐个真实调用 BPS 模型，记录是否可用与输出。

通过本地网关 http://127.0.0.1:8790/v1/responses 发起流式请求，
解析最终事件判定成功，并提取返回的模型与 token 用量。
"""
import json
import sys
import time
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
API = "http://127.0.0.1:8790/v1/responses"

MODELS = [
    "gpt-5.6-luna",
    "gpt-5.6-terra",
    "gpt-5.6-sol",
    "gpt-6-astra",
    "gpt-6-luna",
    "gpt-6-sol",
    "gpt-6.1-sol",
]

PROMPT = "只回复两个字：可以"


def call(model, key, timeout=120):
    body = {
        "model": model,
        "input": PROMPT,
        "stream": True,
        "reasoning": {"effort": "low"},
    }
    started = time.time()
    result = {
        "model": model, "ok": False, "text": "", "reported": "",
        "usage": None, "error": "", "elapsed": 0.0,
    }
    try:
        with requests.post(
            API,
            headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
            json=body, stream=True, timeout=timeout,
        ) as r:
            if r.status_code != 200:
                result["error"] = f"HTTP {r.status_code}: {r.text[:200]}"
                return result
            terminal = ""
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
                et = ev.get("type", "")
                if et == "response.output_text.delta":
                    result["text"] += ev.get("delta", "")
                resp = ev.get("response") or {}
                if et in ("response.completed", "response.incomplete", "response.failed", "error"):
                    terminal = et
                    result["reported"] = resp.get("model", "")
                    result["usage"] = resp.get("usage")
                    if et in ("response.failed", "error"):
                        err = (resp.get("error") or {})
                        result["error"] = f"{err.get('code','')}: {err.get('message','')}"
            result["ok"] = terminal == "response.completed"
            if not result["ok"] and not result["error"]:
                result["error"] = f"terminal={terminal or 'none'}"
    except Exception as e:
        result["error"] = f"{type(e).__name__}: {str(e)[:160]}"
    finally:
        result["elapsed"] = round(time.time() - started, 1)
    return result


def main():
    key = (DATA / "modeltest-key.txt").read_text(encoding="utf-8").strip()
    rows = []
    for m in MODELS:
        r = call(m, key)
        rows.append(r)
        status = "OK  " if r["ok"] else "FAIL"
        detail = (r["text"].strip()[:24] or r["error"][:90])
        print(f"{status} {m:16s} {r['elapsed']:5.1f}s  {detail}")
        if r["usage"]:
            u = r["usage"]
            print(f"      usage: in={u.get('input_tokens')} out={u.get('output_tokens')} "
                  f"total={u.get('total_tokens')} reported={r['reported']}")
    (DATA / "model-test-result.json").write_text(
        json.dumps(rows, ensure_ascii=False, indent=2), encoding="utf-8")
    ok = sum(1 for r in rows if r["ok"])
    print(f"\nSUMMARY: {ok}/{len(rows)} models usable")
    sys.exit(0 if ok == len(rows) else 1)


if __name__ == "__main__":
    main()
