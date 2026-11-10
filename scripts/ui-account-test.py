# -*- coding: utf-8 -*-
"""验证账号「测试」弹窗：模型下拉、默认提示词、发送后展示真实结果、再次发送先清空。"""
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
        page = browser.new_page(viewport={"width": 1280, "height": 950})
        errs = []
        page.on("pageerror", lambda e: errs.append(str(e)))
        page.on("console", lambda m: errs.append(m.text) if m.type == "error" and "401" not in m.text else None)
        page.on("dialog", lambda d: d.accept())

        page.goto("http://127.0.0.1:8791/admin/", wait_until="networkidle")
        page.fill("#login-password", pw)
        page.click("#login-form button[type=submit]")
        page.wait_for_selector("#view-main:not(.hidden)", timeout=8000)
        page.click('button[data-view="accounts"]')
        page.wait_for_selector("#accounts-table tbody tr", timeout=8000)

        row = page.locator("#accounts-table tbody tr").first
        check("账号行有测试按钮", row.locator('button:has-text("测试")').count() == 1)
        row.locator('button:has-text("测试")').click()
        page.wait_for_selector("#modal-overlay:not(.hidden)", timeout=8000)
        check("弹窗打开", page.is_visible("#test-model"))

        # 模型下拉填充
        page.wait_for_function(
            "() => { const s = document.querySelector('#test-model'); return s && s.options.length > 1 && !s.disabled; }",
            timeout=20000)
        opts = page.locator("#test-model option").count()
        check("模型下拉已加载", opts >= 7, f"options={opts}")
        first_val = page.locator("#test-model").input_value()
        check("默认选中模型", bool(first_val), first_val)

        # 默认提示词
        check("提示词默认 hi", (page.input_value("#test-prompt") or "").strip() == "hi",
              page.input_value("#test-prompt"))

        # 发送：等待真实结果
        page.click("#modal .btn.primary")
        page.wait_for_function(
            "() => { const r = document.querySelector('#test-result'); return r && !r.classList.contains('hidden'); }",
            timeout=180000)
        text1 = page.text_content("#test-result") or ""
        ok1 = "输出" in text1 and "返回模型" in text1
        check("发送后展示真实结果", ok1, text1[:110].replace("\n", " "))
        page.screenshot(path=str(DATA / "ui-account-test.png"))

        # 再次发送：结果区应先被清空再填入（用请求耗时验证，内容应重新出现）
        page.fill("#test-prompt", "say OK only")
        page.click("#modal .btn.primary")
        # 点击瞬间结果区应被清空
        cleared = page.evaluate(
            "() => { const r = document.querySelector('#test-result'); "
            "return r.classList.contains('hidden') && r.children.length === 0; }")
        check("再次发送先清空旧结果", cleared)
        page.wait_for_function(
            "() => { const r = document.querySelector('#test-result'); return r && !r.classList.contains('hidden'); }",
            timeout=180000)
        text2 = page.text_content("#test-result") or ""
        check("第二次结果已刷新", "输出" in text2, text2[:110].replace("\n", " "))

        page.click("#modal .modal-actions button:has-text('关闭')")
        page.wait_for_selector("#modal-overlay", state="hidden")
        check("无页面 JS 错误", not errs, " | ".join(errs[:2]))
        browser.close()

    print(f"\nSUMMARY: {sum(results)}/{len(results)} passed")
    sys.exit(0 if all(results) else 1)


if __name__ == "__main__":
    main()
