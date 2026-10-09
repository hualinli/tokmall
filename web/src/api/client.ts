import createClient from 'openapi-fetch'
import type { paths } from './types'
import { useAuthStore } from '../lib/auth-store'
import { refreshAccessToken } from '../lib/auth-refresh'

/**
 * 契约优先：client 类型完全来自 openapi.yaml 生成的 paths。
 * 生产环境通过 VITE_API_BASE_URL 指向真实后端（同源部署时留空）。
 */
export const API_BASE_URL = import.meta.env.VITE_API_BASE_URL ?? ''

export const client = createClient<paths>({
  baseUrl: API_BASE_URL,
})

/**
 * 认证中间件：
 * 1. 自动注入 Authorization: Bearer <access_token>
 * 2. 401 → singleflight 刷新 → 用新 token 重试一次
 *
 * 为什么重试安全：401 意味着请求在 auth 中间件就被拒绝，业务逻辑从未执行。
 * 为什么缓存 body：fetch 首次调用会消费 Request 的 body 流，重试前必须先
 * 把字节存下来（onRequest 阶段、body 未被消费时 clone 读取）。
 */
client.use({
  async onRequest({ request }) {
    const { accessToken } = useAuthStore.getState()
    let cachedBody: ArrayBuffer | undefined
    if (request.method !== 'GET' && request.body) {
      cachedBody = await request.clone().arrayBuffer()
      requestRetryBodies.set(request, cachedBody)
    }
    const next = new Request(request)
    if (accessToken) {
      next.headers.set('Authorization', `Bearer ${accessToken}`)
    }
    return next
  },
  async onResponse({ response, request, schemaPath }) {
    if (response.status !== 401) return
    // auth 端点自身不重试：login 失败是密码错，refresh/logout 失败是轮换失败
    if (schemaPath.startsWith('/api/v1/auth/')) return
    try {
      const token = await refreshAccessToken()
      const headers = new Headers(request.headers)
      headers.set('Authorization', `Bearer ${token}`)
      const body = requestRetryBodies.get(request)
      const retried = new Request(request, { headers, body })
      return fetch(retried)
    } catch {
      // 刷新失败（已清空登录态）：原样透传 401，由调用方展示
      return
    }
  },
})

// 重试 body 缓存：请求对象 → body 字节。仅小 JSON（上传直传 S3 不走此通道），内存可忽略。
const requestRetryBodies = new WeakMap<Request, ArrayBuffer>()
