import { test, expect, type Page } from '@playwright/test'
import { fileURLToPath } from 'url'
import { dirname, join } from 'path'
import { loginViaUi, registerUser } from './utils'

const fixtureDir = join(dirname(fileURLToPath(import.meta.url)), 'fixtures')

/**
 * 售后退款全链路（后端状态机）：买家申请 → 商家同意 → 订单 REFUNDED + 库存回补。
 *
 * 为什么单独一个 spec：退款链路涉及「商家建货 → 买家下单支付 → 双方切换身份裁决」，
 * 步骤长且状态下沉在订单上，放在 commerce.spec 里会让那条主链路更难定位失败。
 */
test('退款：买家申请 → 商家同意 → 库存回补、订单已退款', async ({ page }) => {
  test.setTimeout(120_000)

  // ---------- 商家：商品 + 规格 + 库存 ----------
  const author = await registerUser(page)
  await loginViaUi(page, author.username, author.password)

  await page.goto('/me')
  const productTitle = `退款商品 ${Date.now()}`
  await page.getByPlaceholder('商品名称').fill(productTitle)
  await page.getByRole('button', { name: '创建商品' }).click()
  await expect(page.getByRole('option', { name: new RegExp(productTitle) })).toHaveCount(1)
  const productId = Number(await page.locator('select').first().inputValue())
  await page.getByPlaceholder('价格（元）').fill('8.00')
  await page.getByRole('button', { name: '添加规格' }).click()
  await page.getByPlaceholder('入库数量').fill('5')
  await page.getByRole('button', { name: '补库存' }).click()
  await expect(page.getByText('当前库存 5')).toBeVisible()
  // 规格下拉在补库存后一定存在（补库存用的就是它）
  const skuId = Number(await page.locator('select').nth(1).inputValue())

  // ---------- 商家：发视频并挂商品 ----------
  const videoTitle = `退款视频 ${Date.now()}`
  await page.goto('/upload')
  await page.setInputFiles('input[accept*="video"]', join(fixtureDir, 'sample-video.mp4'))
  await page.setInputFiles('input[accept*="image"]', join(fixtureDir, 'sample-cover.png'))
  await page.fill('input[placeholder*="标题"]', videoTitle)
  await page.getByRole('button', { name: '上传', exact: true }).click()
  await expect(page.getByText('视频已上传，待发布')).toBeVisible({ timeout: 30_000 })
  await page.getByRole('link', { name: '去发布' }).click()
  await page.getByRole('button', { name: '发布', exact: true }).click()

  const authorToken = await accessToken(page)
  const mine = await page.request.get('/api/v1/videos/me', {
    headers: { Authorization: `Bearer ${authorToken}` },
  })
  expect(mine.ok()).toBe(true)
  const videoId = (await mine.json()).items[0].video_id as number
  await page.goto(`/video/${videoId}`)
  await page.getByRole('button', { name: '挂商品' }).click()
  await expect(page.getByRole('link', { name: new RegExp(productTitle) })).toBeVisible()

  // ---------- 买家：下单 + 模拟支付 ----------
  await page.getByRole('button', { name: '退出' }).click()
  const buyer = await registerUser(page)
  await loginViaUi(page, buyer.username, buyer.password)
  await page.goto(`/video/${videoId}`)
  await page.getByRole('link', { name: new RegExp(productTitle) }).click()
  await page.getByRole('button', { name: '下单并模拟支付' }).click()
  await expect(page.getByText(/已模拟支付成功/)).toBeVisible()

  // ---------- 买家：申请退款 ----------
  await page.goto('/orders')
  await expect(page.getByText('待发货')).toBeVisible()
  await page.getByRole('button', { name: '申请退款' }).click()
  await page.getByPlaceholder('退款原因').fill('尺寸不合适')
  await page.getByRole('button', { name: '提交申请' }).click()
  // 退款中角标 + 订单状态文案都为「退款中」（getByText 是子串匹配，用 first 取角标）
  await expect(page.getByText('退款中').first()).toBeVisible()

  // 订单详情页也应展示退款单（404 REFUND_NOT_FOUND 被当作「无退款」，不该报错）
  await page.getByRole('link', { name: productTitle }).click()
  await expect(page.getByText('售后 · 待商家处理')).toBeVisible()
  await expect(page.getByText('申请原因：尺寸不合适')).toBeVisible()

  // ---------- 商家：同意退款 ----------
  await page.getByRole('button', { name: '退出' }).click()
  await loginViaUi(page, author.username, author.password)
  await page.goto('/orders')
  await page.getByRole('button', { name: '我销售的' }).click()
  await expect(page.getByText('退款中').first()).toBeVisible()
  await page.getByRole('button', { name: '同意退款' }).click()
  await expect(page.getByText('已退款').first()).toBeVisible()

  // 未发货（PAID）退款必须回补库存：下单扣 1 → 4，同意退款后回到 5
  const stock = await page.request.get(`/api/v1/products/${productId}/skus/${skuId}/stock`, {
    headers: { Authorization: `Bearer ${await accessToken(page)}` },
  })
  expect(stock.ok()).toBe(true)
  expect((await stock.json()).quantity).toBe(5)

  // ---------- 买家：看到已退款 ----------
  await page.getByRole('button', { name: '退出' }).click()
  await loginViaUi(page, buyer.username, buyer.password)
  await page.goto('/orders')
  await expect(page.getByText('已退款').first()).toBeVisible()
})

/** 从 localStorage 的持久化登录态取 access token（与页面内请求保持一致） */
function accessToken(page: Page): Promise<string> {
  return page.evaluate(
    () => JSON.parse(localStorage.getItem('tokmall-auth') ?? '{}').state?.accessToken as string,
  )
}
