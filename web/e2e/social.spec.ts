import { test, expect, type Page } from '@playwright/test'
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
    // 按钮文案里的点赞数是 formatCount 缩写（1.0k / 3.4万），Number() 解析得到 NaN：
    // 状态断言走 aria-pressed，计数断言走服务端原值（详情接口的 like_count）
    const videoId = Number(await like.getAttribute('data-video-id'))
    expect(videoId).toBeGreaterThan(0)
    const before = await serverLikeCount(page, videoId)

    await like.click()
    await expect(like).toHaveAttribute('aria-pressed', 'true')
    await expect
      .poll(() => like.locator('svg').evaluate((s) => getComputedStyle(s).fill))
      .not.toBe('none') // 红心
    await expect.poll(() => serverLikeCount(page, videoId)).toBe(before + 1)
    expect(await serverLiked(page, videoId)).toBe(true)

    // 取消
    await like.click()
    await expect(like).toHaveAttribute('aria-pressed', 'false')
    await expect
      .poll(() => like.locator('svg').evaluate((s) => getComputedStyle(s).fill))
      .toBe('none')
    await expect.poll(() => serverLikeCount(page, videoId)).toBe(before)
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

    const serverFollowing = () => page.evaluate(async (uid) => {
      const stored = JSON.parse(localStorage.getItem('tokmall-auth')!)
      const res = await fetch(`/api/v1/users/${uid}`, {
        headers: { Authorization: `Bearer ${stored.state.accessToken}` },
      })
      return (await res.json()).is_following
    }, authorId)
    // 按钮先乐观更新，必须等写入事实，不能把 UI 文案当作事务已提交。
    await expect.poll(serverFollowing).toBe(true)

    await fb.click()
    await expect(fb).toContainText('+ 关注')
    await expect.poll(serverFollowing).toBe(false)
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

/** 服务端点赞数（视频详情是公开接口，不需要 token） */
function serverLikeCount(page: Page, videoId: number): Promise<number> {
  return page.evaluate(async (id) => {
    const res = await fetch(`/api/v1/videos/${id}`)
    return (await res.json()).like_count as number
  }, videoId)
}

/** 服务端「我是否已赞」（is_liked 是个性化字段，必须带 token） */
function serverLiked(page: Page, videoId: number): Promise<boolean> {
  return page.evaluate(async (id) => {
    const stored = JSON.parse(localStorage.getItem('tokmall-auth')!)
    const res = await fetch(`/api/v1/videos/${id}`, {
      headers: { Authorization: `Bearer ${stored.state.accessToken}` },
    })
    return (await res.json()).is_liked as boolean
  }, videoId)
}
