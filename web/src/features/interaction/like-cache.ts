import type { InfiniteData, QueryClient } from '@tanstack/react-query'
import type { components } from '../../api/types'

type FeedResult = components['schemas']['FeedResult']

/**
 * 点赞乐观更新的缓存同步：
 * feed / 用户作品列表都是 InfiniteData<FeedResult> 结构，
 * 找到对应 video_id 的条目原地更新 like_count（结构共享，零拷贝成本）。
 */
export function updateFeedResultCount(
  qc: QueryClient,
  key: unknown[],
  videoId: number,
  delta: number,
) {
  qc.setQueryData<InfiniteData<FeedResult>>(key, (old) => {
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

/** 详情页缓存更新：like_count + is_liked */
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
