import { test, expect } from '@playwright/test'
import { registerUser, loginViaUi } from './utils'

test.describe('互动', () => {
  test('未登录点赞 → 跳登录带 redirect → 登录后回跳', async ({ page }) => {
    const { username, password } = await registerUser(page)
    await page.goto('/')
    await page.locator('[data-testid="like-button"]').first().click()
    await expect(page).toHaveURL(/\/login\?redirect=/)
    await loginViaUi(page, username, password)
    // 登录后回跳 Feed（cursor 可能已被同步进 URL）
    await expect
      .poll(() => new URL(page.url()).pathname)
      .toBe('/')
  })

  test('点赞乐观更新 → 服务端确认 → 取消', async ({ page }) => {
    const { username, password } = await registerUser(page)
    await loginViaUi(page, username, password)

    const like = page.locator('[data-testid="like-button"]').first()
    const before = Number((await like.textContent())?.trim())
    await like.click()
    await expect
      .poll(() => like.locator('svg').evaluate((s) => getComputedStyle(s).fill))
      .not.toBe('none') // 红心
    const after = Number((await like.textContent())?.trim())
    expect(after).toBe(before + 1)

    // 服务端确认（带 token 从页面内请求）
    const serverLiked = await page.evaluate(async () => {
      const stored = JSON.parse(localStorage.getItem('tokmall-auth')!)
      const res = await fetch('/api/v1/feed?limit=1')
      const feed = await res.json()
      const detail = await fetch(`/api/v1/videos/${feed.items[0].video_id}`, {
        headers: { Authorization: `Bearer ${stored.state.accessToken}` },
      })
      return (await detail.json()).is_liked
    })
    expect(serverLiked).toBe(true)

    // 取消
    await like.click()
    await expect
      .poll(() => like.locator('svg').evaluate((s) => getComputedStyle(s).fill))
      .toBe('none')
  })

  test('关注 → 服务端确认 → 取关', async ({ page }) => {
    const { username, password } = await registerUser(page)
    await loginViaUi(page, username, password)

    const feed = await (await page.request.get('/api/v1/feed?limit=1')).json()
    const authorId = feed.items[0].author_id
    await page.goto(`/user/${authorId}`)
    const fb = page.locator('[data-testid="follow-button"]')
    await expect(fb).toBeVisible()
    await fb.click()
    await expect(fb).toContainText('已关注')

    const serverFollowing = await page.evaluate(async (uid) => {
      const stored = JSON.parse(localStorage.getItem('tokmall-auth')!)
      const res = await fetch(`/api/v1/users/${uid}`, {
        headers: { Authorization: `Bearer ${stored.state.accessToken}` },
      })
      return (await res.json()).is_following
    }, authorId)
    expect(serverFollowing).toBe(true)

    await fb.click()
    await expect(fb).toContainText('+ 关注')
  })

  test('用户主页展示资料/作品/关注按钮，自己主页无关注按钮', async ({ page }) => {
    const { username, password } = await registerUser(page)
    await loginViaUi(page, username, password)
    await page.goto('/me')
    await expect(page.locator('text=我的视频')).toBeVisible()
    // 访问自己主页 → 无关注按钮
    const myId = await page.evaluate(() => {
      const stored = JSON.parse(localStorage.getItem('tokmall-auth')!)
      return stored.state.user.user_id
    })
    await page.goto(`/user/${myId}`)
    await expect(page.locator('[data-testid="follow-button"]')).toHaveCount(0)
    await expect(page.locator('text=（我）')).toBeVisible()
  })
})
