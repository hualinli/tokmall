import { client } from '../api/client'
import { useAuthStore } from './auth-store'

/**
 * singleflight 刷新队列：
 * N 个并发请求同时 401 → 只发一次 /auth/refresh，所有人共享新 token。
 * 刷新失败（refresh 过期/黑名单）→ 清空本地态（相当于登出）。
 *
 * 与 client.ts 构成延迟循环引用：两者只在函数体内互用，模块初始化无依赖，安全。
 */
let inflight: Promise<string> | null = null

export function refreshAccessToken(): Promise<string> {
  if (!inflight) {
    inflight = doRefresh().finally(() => {
      inflight = null
    })
  }
  return inflight
}

async function doRefresh(): Promise<string> {
  const { refreshToken } = useAuthStore.getState()
  if (!refreshToken) {
    useAuthStore.getState().clear()
    throw new Error('no refresh token')
  }
  const res = await client.POST('/api/v1/auth/refresh', {
    body: { refresh_token: refreshToken },
  })
  if (!res.data) {
    // 轮换失败：refresh token 已被拉黑/过期，只能重新登录
    useAuthStore.getState().clear()
    throw new Error('refresh token invalid')
  }
  useAuthStore.getState().setAuth(res.data)
  return res.data.access_token
}
