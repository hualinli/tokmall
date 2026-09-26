import { test, expect } from '@playwright/test'
import { fileURLToPath } from 'url'
import { dirname, join } from 'path'
import { loginViaUi, registerUser } from './utils'

const fixtureDir = join(dirname(fileURLToPath(import.meta.url)), 'fixtures')

test('视频挂商品 → 下单支付 → 发货完成 → 作者统计', async ({ page }) => {
  test.setTimeout(90_000)
  const author = await registerUser(page)
  await loginViaUi(page, author.username, author.password)

  await page.goto('/me')
  const productTitle = `带货商品 ${Date.now()}`
  await page.getByPlaceholder('商品名称').fill(productTitle)
  await page.getByRole('button', { name: '创建商品' }).click()
  await expect(page.getByRole('option', { name: new RegExp(productTitle) })).toHaveCount(1)
  const productId = Number(await page.locator('select').first().inputValue())
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
  await page.goto('/orders')
  await expect(page.getByRole('link', { name: productTitle })).toBeVisible()
  await expect(page.getByText('待发货')).toBeVisible()
  await page.getByRole('link', { name: productTitle }).click()
  await expect(page.getByRole('heading', { name: '订单详情' })).toBeVisible()
  await expect(page.getByText('合计：¥12.50')).toBeVisible()

  await page.getByRole('button', { name: '退出' }).click()
  await loginViaUi(page, author.username, author.password)
  await page.goto('/orders')
  await page.getByRole('button', { name: '我销售的' }).click()
  await expect(page.getByText(productTitle)).toBeVisible()
  await page.getByRole('button', { name: '模拟发货' }).click()
  await expect(page.getByText('待确认收货')).toBeVisible()

  await page.getByRole('button', { name: '退出' }).click()
  await loginViaUi(page, buyer.username, buyer.password)
  await page.goto('/orders')
  await page.getByRole('button', { name: '确认收货' }).click()
  await expect(page.getByText('已完成')).toBeVisible()

  await page.getByRole('button', { name: '退出' }).click()
  await loginViaUi(page, author.username, author.password)
  await page.goto('/me')
  await expect(page.getByText('¥12.50', { exact: true })).toBeVisible()

  const authorToken = await page.evaluate(() => JSON.parse(localStorage.getItem('tokmall-auth') ?? '{}').state?.accessToken)
  const offSale = await page.request.post(`/api/v1/products/${productId}/status`, {
    headers: { Authorization: `Bearer ${authorToken}` }, data: { status: 'OFF_SALE' },
  })
  expect(offSale.ok()).toBe(true)
  await page.goto(`/video/${videoId}`)
  await expect(page.getByText('已下架 · 不可购买')).toBeVisible()
  await page.getByRole('link', { name: new RegExp(productTitle) }).click()
  await expect(page.getByRole('button', { name: '下单并模拟支付' })).toBeDisabled()
})
