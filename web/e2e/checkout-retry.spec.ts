import { test, expect } from '@playwright/test'

test('过期订单重新购买使用新幂等键，网络重试保留原键', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('tokmall-auth', JSON.stringify({
    state: { accessToken: 'test-token', refreshToken: null, user: { user_id: 1, username: 'buyer', email: 'buyer@test.local', bio: '', avatar_key: null, avatar_url: null } }, version: 0,
  })))
  const keys: string[] = []
  let payAttempts = 0
  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/api/v1/products/1') {
      await route.fulfill({ json: { product_id: 1, title: '重试商品', description: '', merchant_id: 2, status: 'ON_SALE', cover_url: null, skus: [{ sku_id: 1, spec: '默认', price: 1000, status: 'ACTIVE' }] } })
    } else if (path === '/api/v1/orders') {
      keys.push(route.request().postDataJSON().idempotency_key)
      await route.fulfill({ json: { order_id: keys.length < 3 ? 1 : 2, order_no: 'test', status: 'PENDING_PAYMENT', total_amount: 1000, original_amount: 1000, discount_amount: 0, expires_at: new Date().toISOString(), item: {}, replayed: keys.length === 2 } })
    } else if (path.endsWith('/pay')) {
      payAttempts++
      if (payAttempts === 1) await route.abort('failed')
      else if (payAttempts === 2) await route.fulfill({ status: 409, json: { error: { code: 'ORDER_EXPIRED', message: '订单已过期' } } })
      else await route.fulfill({ json: { status: 'PAID' } })
    } else await route.fulfill({ json: {} })
  })
  await page.goto('/product/1')
  const buy = page.getByRole('button', { name: '下单并模拟支付' })
  await buy.click()
  await expect(buy).toBeEnabled()
  await buy.click()
  await page.getByRole('button', { name: '重新购买' }).click()
  await expect(page.getByText(/已模拟支付成功/)).toBeVisible()
  expect(keys).toHaveLength(3)
  expect(keys[0]).toBe(keys[1])
  expect(keys[2]).not.toBe(keys[1])
})
