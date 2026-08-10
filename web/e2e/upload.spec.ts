import { test, expect } from '@playwright/test'
import { registerUser, loginViaUi } from './utils'
import { fileURLToPath } from 'url'
import { dirname, join } from 'path'

const FIXTURE = join(dirname(fileURLToPath(import.meta.url)), 'fixtures', 'sample-video.mp4')

test.describe('上传与发布', () => {
  test('三步上传 → 我的视频待发布 → 发布 → Feed 可见', async ({ page }) => {
    const { username, password } = await registerUser(page)
    await loginViaUi(page, username, password)
    const title = `e2e 上传 ${Date.now().toString().slice(-6)}`

    // 上传页：选文件 + 封面 + 标题
    await page.goto('/upload')
    await page.setInputFiles('input[type="file"]', FIXTURE)
    await page.setInputFiles('input[accept*="image"]', join(dirname(FIXTURE), 'sample-cover.png'))
    await expect(page.locator('img[alt="封面预览"]')).toBeVisible()
    await page.fill('input[placeholder*="标题"]', title)
    await page.click('button:has-text("上传")')

    // 成功态
    await expect(page.locator('text=视频已上传，待发布')).toBeVisible({ timeout: 30_000 })

    // 我的视频：待发布徽章
    await page.click('a:has-text("去发布")')
    await expect(page.locator(`text=${title}`)).toBeVisible()
    await expect(page.getByText('待发布', { exact: true })).toBeVisible()

    // 发布 → 已发布
    await page.click('button:has-text("发布")')
    await expect(page.getByText('已发布', { exact: true })).toBeVisible()

    // Feed 可见（时间序最新 → 顶部），且带封面
    await page.goto('/')
    await expect(page.locator(`text=${title}`)).toBeVisible({ timeout: 15_000 })
    const feedCover = await page.evaluate(async () => {
      const res = await fetch('/api/v1/feed?limit=1')
      return !!(await res.json()).items[0].cover_url
    })
    expect(feedCover).toBe(true)
  })

  test('超小视频被大小校验拦截', async ({ page }) => {
    const { username, password } = await registerUser(page)
    await loginViaUi(page, username, password)
    await page.goto('/upload')
    // 假 mp4（mime 正确但内容极小）→ 过 dropzone，被 1MB 下限拦截
    await page.setInputFiles('input[type="file"]', {
      name: 'fake.mp4',
      mimeType: 'video/mp4',
      buffer: Buffer.from('tiny content, not a real video'),
    })
    await page.fill('input[placeholder*="标题"]', 'fake')
    await page.click('button:has-text("上传")')
    await expect(page.locator('text=文件大小需在 1MB - 512MB 之间')).toBeVisible()
  })
})
