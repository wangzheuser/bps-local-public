# -*- coding: utf-8 -*-
"""下载 ModelTrace 指纹库到 data/unified_bank.json。

指纹库约 900KB，含 18 个模型的统计指纹，因此不纳入版本控制。
归因检测与 Go 端回归基准都需要它。
"""
import sys
import urllib.request
from pathlib import Path

URL = "https://raw.githubusercontent.com/xqy2006/ModelTrace/main/data/unified_bank.json"
DEST = Path(__file__).resolve().parent.parent / "data" / "unified_bank.json"


def main():
    if DEST.exists() and "--force" not in sys.argv:
        print(f"已存在: {DEST} ({DEST.stat().st_size} 字节)")
        print("加 --force 可重新下载")
        return 0
    DEST.parent.mkdir(parents=True, exist_ok=True)
    print(f"下载 {URL}")
    try:
        with urllib.request.urlopen(URL, timeout=120) as r:
            raw = r.read()
    except Exception as e:
        print(f"下载失败: {e}", file=sys.stderr)
        return 1
    if not raw.startswith(b"{"):
        print("响应不是 JSON", file=sys.stderr)
        return 1
    DEST.write_bytes(raw)
    print(f"已保存: {DEST} ({len(raw)} 字节)")
    print("\n启动服务前设置:")
    print(f"  BPS_MODELTRACE_BANK={DEST}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
