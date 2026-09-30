import type { InfiniteData, QueryClient } from '@tanstack/react-query'
import type { components } from '../../api/types'

type FeedResult = components['schemas']['FeedResult']

/**
 * 点赞乐观更新的缓存同步：
 * feed / 用户作品列表都是 InfiniteData<FeedResult> 结构，
 * 找到对应 video_id 的条目原地更新 like_count（结构共享，零拷贝成本）。
 *
 * keyPrefix 用「前缀匹配」而不是精确 key：用户作品列表的 key 是
 * ['user-videos', String(userId)]，早期实现按 ['user-videos'] 精确查，
 * 匹配不到任何缓存，作品页的点赞数永远不会更新。
 * setQueriesData 对不存在的 key 不会新建条目，只改已存在的缓存。
 */
export function updateFeedResultCount(
  qc: QueryClient,
  keyPrefix: unknown[],
  videoId: number,
  delta: number,
) {
  qc.setQueriesData<InfiniteData<FeedResult>>({ queryKey: keyPrefix }, (old) => {
    if (!old) return old
    return {
      ...old,
      pages: old.pages.map((p) => ({
        ...p,
        items: p.items.map((it) =>
          it.video_id === videoId ? { ...it, like_count: it.like_count + delta } : it,
        ),
      })),
    }
  })
}

/** 详情页缓存更新：like_count + is_liked（key 与详情页 ['video', id] 一致） */
export function updateDetailLike(
  qc: QueryClient,
  videoId: number,
  delta: number,
  liked: boolean,
) {
  qc.setQueryData<components['schemas']['VideoDetail']>(['video', String(videoId)], (old) =>
    old ? { ...old, like_count: old.like_count + delta, is_liked: liked } : old,
  )
}
