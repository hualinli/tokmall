import { test, expect } from '@playwright/test'

for (const view of ['detail', 'mine'] as const) {
  test(`${view}: 校验等待后自动解锁发布，拒绝有原因且不能发布`, async ({ page }) => {
    await page.addInitScript(() => localStorage.setItem('tokmall-auth', JSON.stringify({
      state: { accessToken: 'test-token', refreshToken: null, user: { user_id: 42, username: 'author', email: 'author@test.local' } }, version: 0,
    })))
    let status = 'PENDING'
    let videoStatus = 'PENDING_REVIEW'
    let reads = 0
    await page.route('**/api/v1/**', async (route) => {
      const path = new URL(route.request().url()).pathname
      const validation = { status, reason: status === 'REJECTED' ? '视频编码不受支持，请转换编码后重新上传。' : null, can_publish: videoStatus === 'PENDING_REVIEW' && (status === 'PASSED' || status === 'SKIPPED') }
      const video = { video_id: 1, author_id: 42, title: '校验测试视频', video_url: '', duration_seconds: 5, status: videoStatus, created_at: new Date().toISOString(), author: { user_id: 42, username: 'author' }, like_count: 0, is_liked: false, product_cards: [], media_validation: validation }
      if (path === '/api/v1/videos/1') { reads++; await route.fulfill({ json: video }) }
      else if (path === '/api/v1/videos/me') { reads++; await route.fulfill({ json: { items: [video], next_cursor: null, has_more: false } }) }
      else if (path === '/api/v1/me') await route.fulfill({ json: { user_id: 42, username: 'author', email: 'author@test.local' } })
      else await route.fulfill({ json: { items: [], has_more: false } })
    })
    await page.goto(view === 'detail' ? '/video/1' : '/me')
    const publish = page.getByRole('button', { name: '发布', exact: true })
    await expect(page.getByText('校验中，请稍候')).toBeVisible()
    await expect(publish).toBeDisabled()
    status = 'PASSED'
    await expect(publish).toBeEnabled({ timeout: 12_000 })
    await expect(page.getByText('校验通过', { exact: true })).toBeVisible()
    expect(reads).toBeGreaterThan(1)
    status = 'REJECTED'
    await page.reload()
    await expect(publish).toBeDisabled()
    await expect(page.getByText('视频编码不受支持，请转换编码后重新上传。')).toBeVisible()
    status = 'SKIPPED'
    await page.reload()
    await expect(publish).toBeEnabled()
    await expect(page.getByText('未进行媒体校验，当前允许发布')).toBeVisible()
    videoStatus = 'PUBLISHED'
    await page.reload()
    await expect(page.getByText('未进行媒体校验', { exact: true })).toBeVisible()
    await expect(page.getByText('当前允许发布', { exact: false })).toHaveCount(0)
    await expect(publish).toHaveCount(0)
  })
}
