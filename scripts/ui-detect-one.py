# -*- coding: utf-8 -*-
"""验证「单独检测」列：按钮渲染、点击置灰、其他行不受影响、完成后本行更新。"""
import sys
import time
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
        page = browser.new_page(viewport={"width": 1400, "height": 950})
        errs = []
        page.on("pageerror", lambda e: errs.append(str(e)))
        page.on("console", lambda m: errs.append(m.text) if m.type == "error" and "401" not in m.text else None)
        page.on("dialog", lambda d: d.accept())

        page.goto("http://127.0.0.1:8791/admin/", wait_until="networkidle")
        page.fill("#login-password", pw)
        page.click("#login-form button[type=submit]")
        page.wait_for_selector("#view-main:not(.hidden)", timeout=8000)
        page.click('button[data-view="models"]')
        page.wait_for_selector("#cap-table tbody tr", timeout=15000)

        # 1) 表头包含新列
        headers = page.locator("#cap-table thead th").all_text_contents()
        check("表头含「单独检测」列", "单独检测" in headers, " | ".join(headers))
        check("新列在最后一列", headers[-1].strip() == "单独检测", headers[-1] if headers else "")

        # 2) 每行都有检测按钮
        rows = page.locator("#cap-table tbody tr")
        n = rows.count()
        btns = page.locator("#cap-table tbody tr .cap-detect-one")
        check("每行都有检测按钮", btns.count() == n, f"rows={n} buttons={btns.count()}")

        # 3) 每行按钮列数一致（新列存在）
        first_cells = page.locator("#cap-table tbody tr").first.locator("td").count()
        check("行单元格数为 7（新增一列）", first_cells == 7, f"cells={first_cells}")

        # 4) 点击第一行按钮：按钮置灰且文案变化，其他行按钮仍可点击
        first_btn = btns.first
        before_text = first_btn.text_content().strip()
        first_btn.click()
        # 立即检查置灰（检测需数十秒）
        page.wait_for_function(
            "() => { const b = document.querySelector('#cap-table tbody tr .cap-detect-one');"
            " return b && b.disabled && b.textContent.indexOf('检测中') >= 0; }",
            timeout=5000)
        disabled_now = page.evaluate(
            "() => document.querySelector('#cap-table tbody tr .cap-detect-one').disabled")
        text_now = page.locator("#cap-table tbody tr .cap-detect-one").first.text_content().strip()
        check("点击后按钮置灰", disabled_now is True, f"disabled={disabled_now}")
        check("点击后文案变为检测中", "检测中" in text_now, text_now)

        # 其他行按钮不应被禁用
        other_enabled = page.evaluate(
            "() => { const bs = [...document.querySelectorAll('#cap-table tbody tr .cap-detect-one')];"
            " return bs.length > 1 ? !bs[1].disabled : null; }")
        check("其他行按钮不受影响", other_enabled is True, f"other_enabled={other_enabled}")

        page.screenshot(path=str(DATA / "ui-detect-one-pending.png"))

        # 5) 等待完成：按钮恢复、该行结论/时间刷新
        old_time = page.locator("#cap-table tbody tr").first.locator("td.cap-time").text_content()
        page.wait_for_function(
            "() => { const b = document.querySelector('#cap-table tbody tr .cap-detect-one');"
            " return b && !b.disabled && b.textContent.trim() === '检测'; }",
            timeout=600000)
        new_time = page.locator("#cap-table tbody tr").first.locator("td.cap-time").text_content()
        verdict = page.locator("#cap-table tbody tr").first.locator(".cap-badge").text_content()
        check("完成后按钮恢复可点击", True)
        check("该行检测时间已刷新", new_time != old_time, f"{old_time} -> {new_time}")
        check("该行结论存在", verdict in ("真实提供", "已降级", "待复测", "未测出"), verdict)

        page.screenshot(path=str(DATA / "ui-detect-one-done.png"))
        check("无页面 JS 错误", not errs, " | ".join(errs[:2]))
        browser.close()

    print(f"\nSUMMARY: {sum(results)}/{len(results)} passed")
    sys.exit(0 if all(results) else 1)


if __name__ == "__main__":
    main()
