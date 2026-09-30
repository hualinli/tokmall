import { describe, expect, it } from 'vitest'
import { safeRedirectPath } from './safe-redirect'

const BASE = 'https://tokmall.test'

describe('safeRedirectPath', () => {
  it('放行站内相对路径并保留 query/hash', () => {
    expect(safeRedirectPath('/', BASE)).toBe('/')
    expect(safeRedirectPath('/me?tab=orders#top', BASE)).toBe('/me?tab=orders#top')
  })

  it('拒绝协议相对 URL（//evil.com 会跳到外站）', () => {
    expect(safeRedirectPath('//evil.com', BASE)).toBeNull()
    expect(safeRedirectPath('//evil.com/path', BASE)).toBeNull()
    expect(safeRedirectPath('///evil.com', BASE)).toBeNull()
  })

  it('拒绝反斜杠变体（浏览器会归一化成 //）', () => {
    expect(safeRedirectPath('/\\evil.com', BASE)).toBeNull()
    expect(safeRedirectPath('/\\/evil.com', BASE)).toBeNull()
    expect(safeRedirectPath('/a\\b', BASE)).toBeNull()
  })

  it('拒绝绝对 URL 与非 http 协议', () => {
    expect(safeRedirectPath('https://evil.com', BASE)).toBeNull()
    expect(safeRedirectPath('javascript:alert(1)', BASE)).toBeNull()
    expect(safeRedirectPath('me', BASE)).toBeNull()
    expect(safeRedirectPath('#top', BASE)).toBeNull()
  })

  it('拒绝空值', () => {
    expect(safeRedirectPath(undefined, BASE)).toBeNull()
    expect(safeRedirectPath(null, BASE)).toBeNull()
    expect(safeRedirectPath('', BASE)).toBeNull()
  })

  it('编码过的斜杠不会逃出同源（浏览器按同源路径处理）', () => {
    expect(safeRedirectPath('/%2F%2Fevil.com', BASE)).toBe('/%2F%2Fevil.com')
  })

  it('没有可用的 origin 时一律拒绝（宁可不回跳）', () => {
    expect(safeRedirectPath('/me')).toBeNull()
  })
})
