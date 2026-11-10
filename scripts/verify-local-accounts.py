# -*- coding: utf-8 -*-
"""验证本地同步来的每个账号都能真实生成。"""
import json
import sys
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parent.parent
BASE = "http://127.0.0.1:8791/admin/api"


def main():
    pw = (ROOT / "secrets" / "admin_password").read_text(encoding="utf-8").strip()
    s = requests.Session()
    r = s.post(f"{BASE}/login", json={"password": pw}, timeout=20)
    r.raise_for_status()
    s.headers.update({"X-CSRF-Token": r.json()["csrf_token"]})

    accounts = s.get(f"{BASE}/accounts", timeout=20).json()["accounts"]
    print(f"共 {len(accounts)} 个账号\n")
    ok_count = 0
    for a in accounts:
        try:
            r = s.post(f"{BASE}/accounts/{a['id']}/test",
                       json={"model": "gpt-5.6-luna", "prompt": "只回复两个字：可用", "effort": ""},
                       timeout=180)
            if r.status_code != 200:
                print(f"FAIL {a['name']:<22} HTTP {r.status_code}: {r.text[:100]}")
                continue
            d = r.json()
            text = (d.get("text") or "").strip()
            print(f"OK   {a['name']:<22} reported={d.get('reported_model'):<16} "
                  f"latency={d.get('latency_ms')}ms text={text[:20]!r}")
            ok_count += 1
        except Exception as e:
            print(f"FAIL {a['name']:<22} {type(e).__name__}: {str(e)[:80]}")

    print(f"\n{ok_count}/{len(accounts)} 个账号可用")
    return 0 if ok_count == len(accounts) else 1


if __name__ == "__main__":
    sys.exit(main())
