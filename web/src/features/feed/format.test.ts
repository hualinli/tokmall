import { describe, expect, it } from 'vitest'
import { formatCount, formatDuration } from './format'

describe('formatCount', () => {
  it('小于 1000 原样输出', () => {
    expect(formatCount(0)).toBe('0')
    expect(formatCount(1)).toBe('1')
    expect(formatCount(999)).toBe('999')
  })

  it('1000 起用 k，保留一位小数（含进位到 10.0k 的边界）', () => {
    expect(formatCount(1000)).toBe('1.0k')
    expect(formatCount(1500)).toBe('1.5k')
    expect(formatCount(9999)).toBe('10.0k')
  })

  it('10000 起用万，1 万以上保留一位小数', () => {
    expect(formatCount(10_000)).toBe('1.0万')
    expect(formatCount(15_000)).toBe('1.5万')
    expect(formatCount(99_999)).toBe('10.0万')
  })

  it('10 万起取整（不再显示小数）', () => {
    expect(formatCount(100_000)).toBe('10万')
    expect(formatCount(105_000)).toBe('11万')
  })
})

describe('formatDuration', () => {
  it('秒 → m:ss', () => {
    expect(formatDuration(0)).toBe('0:00')
    expect(formatDuration(9)).toBe('0:09')
    expect(formatDuration(65)).toBe('1:05')
    expect(formatDuration(3599)).toBe('59:59')
  })
})
