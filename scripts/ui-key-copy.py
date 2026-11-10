# -*- coding: utf-8 -*-
"""验证 API Key 列表：脱敏显示 + 复制按钮写入剪贴板。"""
import sys
from pathlib import Path

from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"


def main():
    pw = (ROOT / "secrets" / "admin_password").read_text(encoding="utf-8").strip()
    results = []

    def check(name, ok, detail=""):
        results.append(ok)
        print(("PASS " if ok else "FAIL ") + name + ((" :: " + str(detail)) if detail else ""))

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        ctx = browser.new_context(
            viewport={"width": 1280, "height": 900},
            permissions=["clipboard-read", "clipboard-write"],
        )
        page = ctx.new_page()
        errs = []
        page.on("pageerror", lambda e: errs.append(str(e)))

        page.goto("http://127.0.0.1:8791/admin/", wait_until="networkidle")
        page.fill("#login-password", pw)
        page.click("#login-form button[type=submit]")
        page.wait_for_selector("#view-main:not(.hidden)", timeout=8000)
        page.click('button[data-view="keys"]')
        page.wait_for_selector("#keys-table tbody tr", timeout=8000)

        row = page.locator("#keys-table tbody tr").filter(has_text="copy-test").first
        check("copy-test 行存在", row.count() == 1)

        # Key 列必须脱敏
        cells = row.locator("td")
        key_text = (cells.nth(1).text_content() or "").strip()
        check("Key 列脱敏显示", key_text.endswith("…") and len(key_text) < 25, key_text)

        # 复制按钮存在
        copy_btn = row.locator('button:has-text("复制")')
        check("复制按钮存在", copy_btn.count() == 1)

        # 点击复制并读取剪贴板
        copy_btn.click()
        page.wait_for_timeout(1200)
        clip = page.evaluate("() => navigator.clipboard.readText()")
        check("剪贴板已写入完整 Key",
              clip.startswith("sk-bps-") and len(clip) == 50, f"len={len(clip)}")

        # 与列表前缀一致
        check("剪贴板与列表前缀一致", clip.startswith(key_text.rstrip("…")), clip[:16])

        # 页面不得出现完整 Key
        body = page.content()
        check("页面未泄露完整 Key", clip not in body)

        check("无页面 JS 错误", not errs, " | ".join(errs[:2]))
        page.screenshot(path=str(DATA / "ui-key-copy.png"))
        browser.close()

    print(f"\nSUMMARY: {sum(results)}/{len(results)} passed")
    sys.exit(0 if all(results) else 1)


if __name__ == "__main__":
    main()
