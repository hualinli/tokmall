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

  test('无限滚动：到底加载下一页且 cursor 进 URL，窗口化 DOM 不膨胀', async ({ page }) => {
    await page.goto('/')
    const scrollBox = page.locator('div.snap-y') // feed 滚动容器（窗口化后 scrollHeight 仍随翻页增长）
    const initialScrollHeight = await scrollBox.evaluate((el) => el.scrollHeight)
    await page.mouse.move(195, 500)
    for (let i = 0; i < 14; i++) {
      await page.mouse.wheel(0, 860)
      await page.waitForTimeout(150)
    }
    // 翻页生效：内容总高度增长（占位撑住，scrollHeight = 屏数 × 屏高）
    await expect
      .poll(() => scrollBox.evaluate((el) => el.scrollHeight))
      .toBeGreaterThan(initialScrollHeight)
    // 窗口化生效：DOM 内 video 恒为窗口大小（当前屏 ± 1 屏 = 最多 3 张真卡片），不随翻页膨胀
    expect(await page.locator('video').count()).toBeLessThanOrEqual(3)
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
