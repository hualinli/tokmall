import { describe, expect, it } from 'vitest'
import { ApiRequestError } from './api-error'
import { httpStatusOf, shouldRetryQuery } from './query-retry'

describe('httpStatusOf', () => {
  it('读 ApiRequestError 的 status', () => {
    expect(httpStatusOf(new ApiRequestError(404, 'not found'))).toBe(404)
  })

  it('读带 response 的错误（openapi-fetch 形态）', () => {
    expect(httpStatusOf({ response: new Response(null, { status: 409 }) })).toBe(409)
    expect(httpStatusOf({ response: { status: 429 } })).toBe(429)
  })

  it('普通 Error / 网络错误没有状态码', () => {
    expect(httpStatusOf(new Error('boom'))).toBeUndefined()
    expect(httpStatusOf(new TypeError('Failed to fetch'))).toBeUndefined()
    expect(httpStatusOf(null)).toBeUndefined()
    expect(httpStatusOf(undefined)).toBeUndefined()
  })
})

describe('shouldRetryQuery', () => {
  it('4xx 不重试（404/409/401/429 都是确定性失败）', () => {
    expect(shouldRetryQuery(0, new ApiRequestError(404, 'not found'))).toBe(false)
    expect(shouldRetryQuery(0, new ApiRequestError(409, 'conflict'))).toBe(false)
    expect(shouldRetryQuery(0, new ApiRequestError(401, 'unauthorized'))).toBe(false)
    expect(shouldRetryQuery(0, new ApiRequestError(429, 'rate limited'))).toBe(false)
  })

  it('5xx 与网络错误重试', () => {
    expect(shouldRetryQuery(0, new ApiRequestError(500, 'internal'))).toBe(true)
    expect(shouldRetryQuery(0, new TypeError('Failed to fetch'))).toBe(true)
  })

  it('最多重试 2 次（含首次共 3 次请求），避免长时间挂在错误页', () => {
    expect(shouldRetryQuery(1, new ApiRequestError(503, 'unavailable'))).toBe(true)
    expect(shouldRetryQuery(2, new ApiRequestError(503, 'unavailable'))).toBe(false)
  })
})
