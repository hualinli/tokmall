import { test, expect } from '@playwright/test'
import { registerUser, loginViaUi } from './utils'

test.describe('认证', () => {
  test('注册即登录 → 刷新保持 → 登出', async ({ page }) => {
    const ts = Date.now().toString().slice(-6)
    const username = `ui${ts}`
    await page.goto('/register')
    await page.fill('input[type="email"]', `ui_${ts}@test.com`)
    await page.fill('input[autocomplete="username"]', username)
    await page.fill('input[autocomplete="new-password"]', 'test1234')
    await page.click('button[type="submit"]')

    // 注册即签发 token → 回首页，header 显示用户名
    await page.waitForURL((u) => u.pathname === '/')
    await expect(page.locator('header')).toContainText(username)

    // 刷新页面 → localStorage 恢复登录态
    await page.reload()
    await expect(page.locator('header')).toContainText(username)

    // 登出 → 回未登录态
    await page.click('button:has-text("退出")')
    await expect(page.locator('header')).toContainText('登录')
  })

  test('错误密码提示且不登录', async ({ page }) => {
    const { username } = await registerUser(page)
    await page.goto('/login')
    await page.fill('input[autocomplete="username"]', username)
    await page.fill('input[autocomplete="current-password"]', 'wrong-password')
    await page.click('button[type="submit"]')
    await expect(page.locator('text=账号或密码错误')).toBeVisible()
    // 仍停留在登录页
    expect(new URL(page.url()).pathname).toBe('/login')
  })

  test('未登录访问 /me 被守卫重定向', async ({ page }) => {
    await page.goto('/me')
    await expect(page).toHaveURL(/\/login/)
  })

  /**
   * 多标签页登录态同步（auth-sync 的 BroadcastChannel 通道）：
   * 第一个标签页全程不刷新，只有跨标签广播能让它掉登录态——
   * 否则 refresh token 轮换会让后刷新的标签页被踢下线。
   */
  test('另一标签页登出后本页同步掉线', async ({ page, context }) => {
    const { username, password } = await registerUser(page)
    await loginViaUi(page, username, password)

    const other = await context.newPage()
    await other.goto('/')
    await expect(other.locator('header')).toContainText(username)

    await other.getByRole('button', { name: '退出' }).click()
    await expect(other.locator('header')).toContainText('登录')
    // page 没有重新加载、也没有重新 hydrate：状态只能来自广播
    await expect(page.locator('header')).toContainText('登录')
    await expect
      .poll(() =>
        page.evaluate(
          () => JSON.parse(localStorage.getItem('tokmall-auth') ?? '{}').state?.accessToken,
        ),
      )
      .toBeNull()
  })
})
