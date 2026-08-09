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
