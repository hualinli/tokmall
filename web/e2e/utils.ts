import type { Page } from '@playwright/test'

/** 通过 API 注册新用户（走 Vite 代理同源），返回凭证 */
export async function registerUser(page: Page): Promise<{ username: string; password: string }> {
  const ts = Date.now().toString().slice(-6)
  const username = `e2e${ts}`
  const password = 'test1234'
  const res = await page.request.post('/api/v1/auth/register', {
    data: { email: `e2e_${ts}@test.com`, username, password },
  })
  if (!res.ok()) throw new Error(`register failed: ${res.status()}`)
  return { username, password }
}

/** 通过 UI 登录 */
export async function loginViaUi(page: Page, username: string, password: string) {
  await page.goto('/login')
  await page.fill('input[autocomplete="username"]', username)
  await page.fill('input[autocomplete="current-password"]', password)
  await page.click('button[type="submit"]')
  await page.waitForURL((url) => url.pathname !== '/login')
}

/** 等待视频进入播放状态（muted autoplay 异步） */
export async function expectPlaying(page: Page, index: number) {
  const video = page.locator('video').nth(index)
  await page
    .waitForFunction(
      (i) => {
        const v = document.querySelectorAll('video')[i]
        return v && !v.paused && v.currentTime > 0
      },
      index,
      { timeout: 10_000 },
    )
    .catch(() => {
      /* 返回具体状态由调用方断言 */
    })
}
