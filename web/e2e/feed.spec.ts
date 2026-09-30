import { test, expect } from '@playwright/test'
import { seedPublishedVideos } from './utils'

// 造 12 条可播放视频（页大小 10 → 必然有第二页），
// 让分页与"滚动切换播放"用例不再依赖开发库里的历史数据。
test.beforeAll(async ({ request }) => {
  await seedPublishedVideos(request, 12)
})

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
    // 边滚边等「高度增长」这一条件，而不是固定滚 14 次 + sleep：
    // 固定等待在后端慢/快时都会假失败（判据应该是翻页真的发生了）。
    await expect
      .poll(
        async () => {
          await page.mouse.wheel(0, 860)
          return scrollBox.evaluate((el) => el.scrollHeight)
        },
        { timeout: 20_000, intervals: [250] },
      )
      .toBeGreaterThan(initialScrollHeight)
    // 窗口化生效：DOM 内 video 恒为窗口大小（当前屏 ± 1 屏 = 最多 3 张真卡片），不随翻页膨胀
    expect(await page.locator('video').count()).toBeLessThanOrEqual(3)
    // cursor 由 effect 在数据到达后写入 URL，同样用 poll 等
    await expect.poll(() => new URL(page.url()).searchParams.get('cursor')).toBeTruthy()
  })

  test('feed 卡片展示作者与点赞数', async ({ page }) => {
    await page.goto('/')
    await expect(page.locator('video').first()).toBeVisible()
    const author = page.locator('text=@').first()
    await expect(author).toBeVisible()
    await expect(page.locator('[data-testid="like-button"]').first()).toBeVisible()
  })
})
