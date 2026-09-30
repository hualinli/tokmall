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
  const token = await requestRefresh(refreshToken)
  if (token) return token

  // 失败不一定是「token 失效」：也可能是另一个标签页刚刚完成轮换，把旧 token 拉黑了。
  // 跨标签同步会把新 token 对广播过来（auth-sync），所以这里用「最新的」值重试一次，
  // 而不是直接清空登录态把用户踢下线。只重试一次，防止失败循环。
  const latest = useAuthStore.getState().refreshToken
  if (latest && latest !== refreshToken) {
    const retried = await requestRefresh(latest)
    if (retried) return retried
  }

  // 轮换真的失败：refresh token 已过期/被拉黑，只能重新登录
  useAuthStore.getState().clear()
  throw new Error('refresh token invalid')
}

/** 发一次 /auth/refresh；失败返回 null，由调用方决定「重试还是登出」 */
async function requestRefresh(refreshToken: string): Promise<string | null> {
  const res = await client.POST('/api/v1/auth/refresh', {
    body: { refresh_token: refreshToken },
  })
  if (!res.data) return null
  useAuthStore.getState().setAuth(res.data)
  return res.data.access_token
}
