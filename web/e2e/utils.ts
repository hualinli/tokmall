import type { APIRequestContext, Page } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const FIXTURE_VIDEO = join(dirname(fileURLToPath(import.meta.url)), 'fixtures', 'sample-video.mp4')

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

/**
 * 通过 API 发布若干条**可播放**的视频。
 *
 * 为什么 e2e 必须自己造数：Feed 的分页与"滚动切换播放"用例需要
 *   1) 至少两页数据（否则无限滚动永远到底），
 *   2) 视频对象真实存在且可解码（开发库里历史种子数据的 RustFS 对象可能已被删除，
 *      浏览器会报 MEDIA_ERR_SRC_NOT_SUPPORTED）。
 * 依赖"上一个 spec 恰好发布过视频"是隐式顺序依赖，单跑本文件必挂。
 */
export async function seedPublishedVideos(request: APIRequestContext, count: number): Promise<void> {
  const ts = Date.now().toString().slice(-6)
  const username = `seed${ts}`
  const password = 'test1234'
  const register = await request.post('/api/v1/auth/register', {
    data: { email: `seed_${ts}@test.com`, username, password },
  })
  if (!register.ok()) throw new Error(`seed register failed: ${register.status()}`)
  const token = (await register.json()).access_token as string
  const auth = { Authorization: `Bearer ${token}` }
  const body = readFileSync(FIXTURE_VIDEO)

  for (let i = 0; i < count; i++) {
    const ticket = await request.post('/api/v1/videos/upload-url', {
      headers: auth,
      data: { filename: `seed-${ts}-${i}.mp4`, file_size: body.length, content_type: 'video/mp4' },
    })
    if (!ticket.ok()) throw new Error(`upload-url failed: ${ticket.status()} ${await ticket.text()}`)
    const { object_key, upload_url } = (await ticket.json()) as { object_key: string; upload_url: string }
    const put = await request.put(upload_url, {
      headers: { 'Content-Type': 'video/mp4' },
      data: body,
    })
    if (!put.ok()) throw new Error(`presigned PUT failed: ${put.status()}`)

    const created = await request.post('/api/v1/videos', {
      headers: auth,
      data: { title: `seed ${ts}-${i}`, object_key, duration_seconds: 5 },
    })
    if (!created.ok()) throw new Error(`create video failed: ${created.status()} ${await created.text()}`)
    const { video_id } = (await created.json()) as { video_id: number }
    const published = await request.post(`/api/v1/videos/${video_id}/publish`, { headers: auth })
    if (!published.ok()) throw new Error(`publish failed: ${published.status()}`)
  }
}
