import { test, expect } from '@playwright/test'
import { fileURLToPath } from 'url'
import { dirname, join } from 'path'
import { loginViaUi, registerUser } from './utils'

const fixtureDir = join(dirname(fileURLToPath(import.meta.url)), 'fixtures')

test('视频挂商品 → 商品卡 → 下单支付 → 作者统计', async ({ page }) => {
  test.setTimeout(90_000)
  const author = await registerUser(page)
  await loginViaUi(page, author.username, author.password)

  await page.goto('/me')
  const productTitle = `带货商品 ${Date.now()}`
  await page.getByPlaceholder('商品名称').fill(productTitle)
  await page.getByRole('button', { name: '创建商品' }).click()
  await expect(page.getByRole('option', { name: new RegExp(productTitle) })).toHaveCount(1)
  await page.getByPlaceholder('价格（元）').fill('12.50')
  await page.getByRole('button', { name: '添加规格' }).click()
  await page.getByPlaceholder('入库数量').fill('10')
  await page.getByRole('button', { name: '补库存' }).click()
  await expect(page.getByText('当前库存 10')).toBeVisible()

  const videoTitle = `带货视频 ${Date.now()}`
  await page.goto('/upload')
  await page.setInputFiles('input[type="file"]', join(fixtureDir, 'sample-video.mp4'))
  await page.setInputFiles('input[accept*="image"]', join(fixtureDir, 'sample-cover.png'))
  await page.fill('input[placeholder*="标题"]', videoTitle)
  await page.getByRole('button', { name: '上传', exact: true }).click()
  await expect(page.getByText('视频已上传，待发布')).toBeVisible({ timeout: 30_000 })
  await page.getByRole('link', { name: '去发布' }).click()
  await page.getByRole('button', { name: '发布', exact: true }).click()
  const videoResponse = await page.request.get('/api/v1/videos/me', {
    headers: { Authorization: `Bearer ${await page.evaluate(() => JSON.parse(localStorage.getItem('tokmall-auth') ?? '{}').state?.accessToken)}` },
  })
  expect(videoResponse.ok()).toBe(true)
  const videoId = (await videoResponse.json()).items[0].video_id as number
  await page.goto(`/video/${videoId}`)
  await page.getByRole('button', { name: '挂商品' }).click()
  await expect(page.getByRole('link', { name: new RegExp(productTitle) })).toBeVisible()

  await page.getByRole('button', { name: '退出' }).click()
  const buyer = await registerUser(page)
  await loginViaUi(page, buyer.username, buyer.password)
  await page.goto(`/video/${videoId}`)
  await page.getByRole('link', { name: new RegExp(productTitle) }).click()
  await page.getByRole('button', { name: '下单并模拟支付' }).click()
  await expect(page.getByText(/已模拟支付成功/)).toBeVisible()

  await page.getByRole('button', { name: '退出' }).click()
  await loginViaUi(page, author.username, author.password)
  await page.goto('/me')
  await expect(page.getByText('¥12.50', { exact: true })).toBeVisible()
})
