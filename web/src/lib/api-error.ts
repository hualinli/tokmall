import type { components } from '../api/types'

type ApiError = components['schemas']['Error']

/**
 * 统一错误文案：后端错误结构 { error: {code,message}, request_id }。
 * request_id 一并展示，方便用户和后端日志对账（呼应后端「错误三层链」设计）。
 */
export function apiErrorMessage(err: ApiError | undefined | null, fallback = '请求失败'): string {
  if (!err) return fallback
  const msg = err.error?.message ?? fallback
  return err.request_id ? `${msg}（request_id: ${err.request_id}）` : msg
}

/**
 * 携带 HTTP 状态的 API 错误。
 * 为什么需要：queryFn 里 `throw new Error(message)` 会把状态码丢掉，
 * React Query 就分不清「404 不存在」（不该重试）与「500/网络抖动」（该重试）。
 */
export class ApiRequestError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiRequestError'
    this.status = status
  }
}

/**
 * 把 openapi-fetch 的失败结果（{ error, response }）转成带状态的错误对象。
 * response 是原始 Response，即使 body 不是预期的错误结构也一定存在。
 */
export function apiRequestError(
  res: { error?: ApiError | null; response: Response },
  fallback?: string,
): ApiRequestError {
  return new ApiRequestError(res.response.status, apiErrorMessage(res.error, fallback))
}
