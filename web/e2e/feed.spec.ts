import { test, expect } from '@playwright/test'

test.describe('Feed', () => {
  test('首屏静音自动播放', async ({ page }) => {
    await page.goto('/')
    const v0 = page.locator('video').first()
    await expect
      .poll(async () => v0.evaluate((v) => ({ paused: v.paused, muted: v.muted })))
      .toEqual({ paused: false, muted: true })
  })

  test('滚动切换活跃视频（前一个暂停，后一个播放）', async ({ page }) => {
    await page.goto('/')
    // 等首屏视频开始播放再滚动（feed 异步加载）
    await expect
      .poll(() => page.locator('video').first().evaluate((v) => !v.paused))
      .toBe(true)
    await page.mouse.move(195, 500)
    await page.mouse.wheel(0, 860)
    const v0 = page.locator('video').nth(0)
    const v1 = page.locator('video').nth(1)
    await expect.poll(() => v0.evaluate((v) => v.paused)).toBe(true)
    await expect.poll(() => v1.evaluate((v) => !v.paused)).toBe(true)
  })

  test('无限滚动：到底加载下一页且 cursor 进 URL', async ({ page }) => {
    await page.goto('/')
    const initial = await page.locator('video').count()
    await page.mouse.move(195, 500)
    for (let i = 0; i < 14; i++) {
      await page.mouse.wheel(0, 860)
      await page.waitForTimeout(150)
    }
    await expect.poll(() => page.locator('video').count()).toBeGreaterThan(initial)
    expect(new URL(page.url()).searchParams.get('cursor')).toBeTruthy()
  })

  test('feed 卡片展示作者与点赞数', async ({ page }) => {
    await page.goto('/')
    await expect(page.locator('video').first()).toBeVisible()
    const author = page.locator('text=@').first()
    await expect(author).toBeVisible()
    await expect(page.locator('[data-testid="like-button"]').first()).toBeVisible()
  })
})
