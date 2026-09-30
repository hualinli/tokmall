import type { QueryClient } from '@tanstack/react-query'
import type { components } from '../../api/types'

type PublicProfile = components['schemas']['PublicProfile']

/**
 * 关注状态的缓存同步（用户主页 queryKey 为 ['user', String(userId)]）。
 *
 * 语义坑：关注别人改变的是「对方」的 followers_count（粉丝数），
 * 与本地的 following_count（我关注了多少人）无关——早期实现改错了字段，
 * 而且 onMutate/onSuccess 各应用一次 delta，导致计数翻倍。
 *
 * 因此这里接「目标状态布尔值」而不是 delta，并按 is_following 做值幂等：
 * 重复调用（乐观 + 成功回调）不会重复计数，只有真正发生翻转才动 followers_count。
 */
export function withFollowState(profile: PublicProfile, following: boolean): PublicProfile {
  if (profile.is_following === following) return profile
  return {
    ...profile,
    is_following: following,
    followers_count: Math.max(0, profile.followers_count + (following ? 1 : -1)),
  }
}

/** 把关注状态写进该用户的资料缓存；缓存不存在时不动（不凭空造条目）。 */
export function updateFollowCache(qc: QueryClient, userId: number, following: boolean) {
  qc.setQueryData<PublicProfile>(['user', String(userId)], (old) =>
    old ? withFollowState(old, following) : old,
  )
}
