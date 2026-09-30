/**
 * openapi-fetch 的失败结果会带原始 Response；我们自己抛的 ApiRequestError 带 status。
 * 两种形态都要能抽出状态码，供 React Query 决定是否重试。
 */
export function httpStatusOf(error: unknown): number | undefined {
  if (typeof error !== 'object' || error === null) return undefined
  const status = (error as { status?: unknown }).status
  if (typeof status === 'number') return status
  const response = (error as { response?: unknown }).response
  if (response instanceof Response) return response.status
  if (typeof response === 'object' && response !== null) {
    const nested = (response as { status?: unknown }).status
    if (typeof nested === 'number') return nested
  }
  return undefined
}

/**
 * React Query 重试策略。
 *
 * 4xx 是确定性拒绝（404 不存在 / 409 状态冲突 / 401 未认证 / 429 限流），
 * 重试既不会成功，还会放大后端压力、拖慢错误提示（默认 3 次退避 ≈ 7s）；
 * 只有 5xx 与网络抖动才值得退避重试。
 */
export function shouldRetryQuery(failureCount: number, error: unknown): boolean {
  if (failureCount >= 2) return false
  const status = httpStatusOf(error)
  if (status !== undefined && status >= 400 && status < 500) return false
  return true
}
