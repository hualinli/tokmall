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
})
