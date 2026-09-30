import { describe, expect, it } from 'vitest'
import { QueryClient } from '@tanstack/react-query'
import type { components } from '../../api/types'
import { updateFollowCache, withFollowState } from './follow-cache'

type PublicProfile = components['schemas']['PublicProfile']

function profile(overrides: Partial<PublicProfile> = {}): PublicProfile {
  return {
    user_id: 7,
    username: 'alice',
    bio: '',
    avatar_url: null,
    following_count: 3,
    followers_count: 10,
    is_following: false,
    is_me: false,
    ...overrides,
  }
}

describe('withFollowState', () => {
  it('关注：只加对方 followers_count，不动自己的 following_count', () => {
    const next = withFollowState(profile(), true)
    expect(next.followers_count).toBe(11)
    expect(next.following_count).toBe(3)
    expect(next.is_following).toBe(true)
  })

  it('取关：followers_count -1', () => {
    const next = withFollowState(profile({ is_following: true, followers_count: 11 }), false)
    expect(next.followers_count).toBe(10)
    expect(next.is_following).toBe(false)
  })

  it('按值幂等：重复应用同一目标状态不会重复计数', () => {
    const once = withFollowState(profile(), true)
    const twice = withFollowState(once, true)
    expect(twice.followers_count).toBe(11)
    // 值没变时直接返回原对象（结构共享）
    expect(twice).toBe(once)
  })

  it('粉丝数不会变成负数（服务端计数抖动时的兜底）', () => {
    const next = withFollowState(profile({ is_following: true, followers_count: 0 }), false)
    expect(next.followers_count).toBe(0)
  })
})

describe('updateFollowCache', () => {
  it('写入用户资料缓存（key 为 [user, String(userId)]）', () => {
    const qc = new QueryClient()
    qc.setQueryData(['user', '7'], profile())
    updateFollowCache(qc, 7, true)
    const cached = qc.getQueryData<PublicProfile>(['user', '7'])
    expect(cached?.followers_count).toBe(11)
    expect(cached?.is_following).toBe(true)
  })

  it('乐观更新 + 成功回调各调一次也只计一次', () => {
    const qc = new QueryClient()
    qc.setQueryData(['user', '7'], profile())
    updateFollowCache(qc, 7, true) // onMutate
    updateFollowCache(qc, 7, true) // onSuccess（服务端返回 following=true）
    expect(qc.getQueryData<PublicProfile>(['user', '7'])?.followers_count).toBe(11)
  })

  it('缓存不存在时不凭空创建条目', () => {
    const qc = new QueryClient()
    updateFollowCache(qc, 99, true)
    expect(qc.getQueryData(['user', '99'])).toBeUndefined()
  })
})
