# -*- coding: utf-8 -*-
"""bps-server 管理 UI 自动化真实测试（Python Playwright）。

覆盖：登录、总览、账号页与授权面板、Key 创建与一次性显示、用量页、登出。
"""
import re
import sys
import time
from pathlib import Path

from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "data"
SECRETS = ROOT / "secrets"

results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(("PASS" if ok else "FAIL") + " " + name + ((" :: " + str(detail)) if detail else ""))


def main():
    password = (SECRETS / "admin_password").read_text(encoding="utf-8").strip()
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        page = browser.new_page(viewport={"width": 1280, "height": 900})
        errors = []
        page.on("pageerror", lambda e: errors.append("pageerror: " + str(e)))
        page.on("console", lambda m: errors.append("console: " + m.text)
                if m.type == "error" and "401" not in m.text else None)
        # 删除/保存等操作使用 window.confirm，测试中一律接受。
        page.on("dialog", lambda d: d.accept())

        try:
            # 1. 登录页
            page.goto("http://127.0.0.1:8791/admin/", wait_until="networkidle")
            check("登录页可见", page.is_visible("#view-login"))
            check("登录页有密码框", page.is_visible("#login-password"))

            # 2. 错误密码提示
            page.fill("#login-password", "wrong-password")
            page.click("#login-form button[type=submit]")
            page.wait_for_selector("#login-error:not(.hidden)", timeout=5000)
            check("错误密码有提示", "密码错误" in page.text_content("#login-error"))

            # 3. 正确登录
            page.fill("#login-password", password)
            page.click("#login-form button[type=submit]")
            page.wait_for_selector("#view-main:not(.hidden)", timeout=5000)
            check("登录成功进入主界面", page.is_visible("#page-overview"))
            page.wait_for_selector("#overview-cards .card", timeout=5000)
            cards = page.locator("#overview-cards .card").count()
            check("总览卡片渲染", cards >= 6, f"cards={cards}")

            # 4. 账号页（先确保至少有一个账号）
            page.click('button[data-view="accounts"]')
            page.wait_for_timeout(600)
            if page.locator("#accounts-table tbody tr").count() == 0:
                page.click("#add-account-btn")
                page.wait_for_selector("#modal-overlay:not(.hidden)")
                page.fill("#modal input[type=text]", "测试账号A")
                page.click("#modal .btn.primary")
                page.wait_for_selector("#modal-overlay:not(.hidden)")
                page.wait_for_function(
                    "() => { const c = document.querySelector('.auth-panel code'); return c && c.textContent.includes('auth.openai.com'); }",
                    timeout=20000)
                page.click("#modal .modal-actions button:has-text('关闭')")
                page.wait_for_selector("#modal-overlay", state="hidden")
                page.wait_for_selector("#accounts-table tbody tr", timeout=8000)
            page.wait_for_selector("#accounts-table tbody tr", timeout=5000)
            row_text = page.text_content("#accounts-table tbody tr")
            check("账号行渲染", bool(row_text and row_text.strip()), (row_text or "")[:60])
            check("账号状态徽章", page.locator("#accounts-table .badge").count() >= 1)
            check("不暴露凭据路径", "-v1.json" not in row_text)

            # 5. 授权面板
            page.click("#accounts-table tbody tr button:has-text('开始授权'), "
                       "#accounts-table tbody tr button:has-text('重新授权'), "
                       "#accounts-table tbody tr button:has-text('查看授权')")
            page.wait_for_selector("#modal-overlay:not(.hidden)", timeout=5000)
            page.wait_for_function(
                "() => { const el = document.querySelector('.auth-panel code'); "
                "return el && el.textContent && el.textContent.includes('auth.openai.com'); }",
                timeout=15000)
            auth_url = page.text_content(".auth-panel code")
            check("授权面板显示真实 URL", "auth.openai.com" in auth_url, auth_url[:70])
            codes = page.locator(".auth-panel .form-row:nth-of-type(2) code")
            user_code = codes.text_content() if codes.count() else ""
            check("授权面板显示用户码", bool(re.match(r"^[A-Z0-9-]{6,}$", (user_code or "").strip())),
                  (user_code or "").strip())
            check("页面不泄露令牌", not re.search(r"access_token", page.content(), re.I))
            page.screenshot(path=str(DATA / "ui-auth-dialog.png"))
            page.click("#modal .modal-actions button:has-text('关闭')")
            page.wait_for_selector("#modal-overlay", state="hidden", timeout=3000)

            # 6b. 删除账号（先建一个临时账号）
            page.click('button[data-view="accounts"]')
            page.wait_for_selector("#accounts-table tbody tr", timeout=5000)
            rows_before_del = page.locator("#accounts-table tbody tr").count()
            page.click("#add-account-btn")
            page.wait_for_selector("#modal-overlay:not(.hidden)")
            page.fill("#modal input[type=text]", "待删除账号")
            page.click("#modal .btn.primary")
            page.wait_for_selector("#modal-overlay:not(.hidden)")
            page.wait_for_function(
                "() => { const c = document.querySelector('.auth-panel code'); return c && c.textContent.includes('auth.openai.com'); }",
                timeout=20000)
            page.click("#modal .modal-actions button:has-text('关闭')")
            page.wait_for_selector("#modal-overlay", state="hidden")
            page.wait_for_function(
                "n => document.querySelectorAll('#accounts-table tbody tr').length === n",
                arg=rows_before_del + 1, timeout=5000)
            check("新增账号用于删除测试", page.locator("#accounts-table tbody tr").count() == rows_before_del + 1)

            # 点击该行的删除按钮并接受确认框（同名残留可能多行，取最后一行）
            del_row = page.locator("#accounts-table tbody tr", has_text="待删除账号").last
            del_row.locator('button:has-text("删除")').click()
            page.wait_for_function(
                "n => document.querySelectorAll('#accounts-table tbody tr').length === n",
                arg=rows_before_del, timeout=8000)
            check("删除账号后列表移除该行",
                  page.locator("#accounts-table tbody tr").count() == rows_before_del)

            # 6c. API Key 页（先确保至少有一个 Key）
            page.click('button[data-view="keys"]')
            page.wait_for_timeout(700)
            if page.locator("#keys-table tbody tr").count() == 0:
                page.click("#add-key-btn")
                page.wait_for_selector("#modal-overlay:not(.hidden)")
                page.fill("#modal input[type=text]", "基线Key")
                page.click("#modal .btn.primary")
                page.wait_for_selector(".key-reveal", timeout=8000)
                page.click("#modal .modal-actions button:has-text('我已保存，关闭')")
                page.wait_for_selector("#modal-overlay", state="hidden")
                page.wait_for_selector("#keys-table tbody tr", timeout=8000)
            page.wait_for_selector("#keys-table tbody tr", timeout=5000)
            key_rows_before = page.locator("#keys-table tbody tr").count()
            key_row = page.text_content("#keys-table tbody tr")
            check("Key 列表显示前缀", "sk-bps-" in key_row, key_row[:60])
            check("Key 列表不含完整明文", not re.search(r"sk-bps-[A-Za-z0-9_-]{43}", key_row))

            # 7. 创建 Key 与一次性显示
            page.click("#add-key-btn")
            page.wait_for_selector("#modal-overlay:not(.hidden)")
            page.fill("#modal input[type=text]", f"ui-test-key-{int(time.time())}")
            page.click("#modal .btn.primary")
            page.wait_for_selector(".key-reveal", timeout=5000)
            revealed = (page.text_content(".key-reveal") or "").strip()
            check("创建后一次性显示完整 Key",
                  revealed.startswith("sk-bps-") and len(revealed) == 50, f"len={len(revealed)}")
            page.screenshot(path=str(DATA / "ui-key-modal.png"))
            page.click("#modal .modal-actions button:has-text('我已保存，关闭')")
            page.wait_for_selector("#modal-overlay", state="hidden")
            # loadKeys is async; wait until the new row actually appears.
            page.wait_for_function(
                "n => document.querySelectorAll('#keys-table tbody tr').length === n",
                arg=key_rows_before + 1, timeout=5000)
            keys_now = page.locator("#keys-table tbody tr").count()
            check("Key 列表新增一行", keys_now == key_rows_before + 1,
                  f"rows={keys_now} before={key_rows_before}")

            # 7b. 删除 Key
            key_rows_before_del = keys_now
            page.locator("#keys-table tbody tr", has_text="ui-test-key").first.locator(
                'button:has-text("删除")').click()
            page.wait_for_function(
                "n => document.querySelectorAll('#keys-table tbody tr').length === n",
                arg=key_rows_before_del - 1, timeout=8000)
            check("删除 Key 后列表移除该行",
                  page.locator("#keys-table tbody tr").count() == key_rows_before_del - 1)

            # 8. 用量页
            page.click('button[data-view="usage"]')
            page.wait_for_timeout(1000)
            check("用量页渲染", page.is_visible("#page-usage"))
            usage_rows = page.locator("#usage-table tbody tr").count()
            check("用量请求表有记录", usage_rows >= 1, f"rows={usage_rows}")
            usage_text = page.text_content("#usage-table tbody tr") if usage_rows else ""
            check("用量行包含 no_account_available", "no_account_available" in usage_text)
            check("用量不含提示词", "只回复 OK" not in page.content())
            page.screenshot(path=str(DATA / "ui-usage.png"))

            # 8b. 设置页：代理检测与保存
            page.click('button[data-view="settings"]')
            page.wait_for_selector("#page-settings:not(.hidden)", timeout=5000)
            check("设置页渲染", page.is_visible("#proxy-input"))
            page.wait_for_function(
                "() => (document.querySelector('#proxy-current').textContent || '').includes('当前生效')",
                timeout=8000)
            current = page.text_content("#proxy-current") or ""
            check("显示当前代理状态", "当前生效" in current, current[:60])

            # 检测不可达代理：应显示失败结果
            page.fill("#proxy-input", "http://127.0.0.1:1")
            page.click("#proxy-check-btn")
            page.wait_for_selector("#proxy-result:not(.hidden)", timeout=20000)
            page.wait_for_function(
                "() => document.querySelector('#proxy-result').classList.contains('err')",
                timeout=20000)
            fail_text = page.text_content("#proxy-result")
            check("不可达代理检测为失败", "代理不可用" in fail_text, fail_text[:80].replace("\n", " "))
            page.screenshot(path=str(DATA / "ui-proxy-fail.png"))

            # 检测可达代理：应显示成功（宿主机 7890 代理）
            page.fill("#proxy-input", "http://127.0.0.1:7890")
            page.click("#proxy-check-btn")
            page.wait_for_function(
                "() => document.querySelector('#proxy-result').classList.contains('ok')",
                timeout=30000)
            ok_text = page.text_content("#proxy-result")
            check("可达代理检测为成功", "代理可用" in ok_text, ok_text[:80].replace("\n", " "))
            page.screenshot(path=str(DATA / "ui-proxy-ok.png"))

            # 保存并立即生效
            page.click("#proxy-save-btn")
            page.wait_for_function(
                "() => (document.querySelector('#proxy-current').textContent || '').includes('7890')",
                timeout=15000)
            saved_text = page.text_content("#proxy-current")
            check("保存后当前代理立即更新", "7890" in saved_text, saved_text[:60])

            # 清除回直连（避免影响后续测试）
            page.fill("#proxy-input", "")
            page.click("#proxy-save-btn")
            page.wait_for_function(
                "() => (document.querySelector('#proxy-current').textContent || '').includes('直连')",
                timeout=15000)
            check("清除代理恢复直连", "直连" in (page.text_content("#proxy-current") or ""))

            # 9. 登出
            page.click("#logout-btn")
            page.wait_for_selector("#view-login:not(.hidden)", timeout=5000)
            check("登出后回到登录页", page.is_visible("#login-password"))

            # 10. JS 错误
            check("无页面 JS 错误", len(errors) == 0, " | ".join(errors[:3]))
        except Exception as e:  # noqa: BLE001
            check("脚本异常", False, str(e))
            try:
                page.screenshot(path=str(DATA / "ui-failure.png"))
            except Exception:
                pass
        finally:
            browser.close()

    failed = [r for r in results if not r[1]]
    print(f"\nSUMMARY: {len(results) - len(failed)}/{len(results)} passed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
