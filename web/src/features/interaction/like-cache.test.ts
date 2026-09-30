import { describe, expect, it } from 'vitest'
import { QueryClient, type InfiniteData } from '@tanstack/react-query'
import type { components } from '../../api/types'
import { updateDetailLike, updateFeedResultCount } from './like-cache'

type FeedResult = components['schemas']['FeedResult']
type VideoDetail = components['schemas']['VideoDetail']

/**
 * 只造「video_id + like_count」两项：helper 只按 video_id 匹配并改 like_count，
 * 其余字段与断言无关，用断言收窄到最小结构即可（避免把整个 FeedItem 抄一遍）。
 */
function pagesOf(counts: Record<number, number>): InfiniteData<FeedResult> {
  return {
    pages: [
      {
        items: Object.entries(counts).map(([videoId, likeCount]) => ({
          video_id: Number(videoId),
          like_count: likeCount,
        })),
      },
    ],
    pageParams: [undefined],
  } as unknown as InfiniteData<FeedResult>
}

function likeCountOf(qc: QueryClient, key: unknown[], index = 0): number | undefined {
  return qc.getQueryData<InfiniteData<FeedResult>>(key)?.pages[0]?.items[index]?.like_count
}

describe('updateFeedResultCount', () => {
  it('前缀匹配到用户作品列表的真实 key [user-videos, userId]', () => {
    const qc = new QueryClient()
    qc.setQueryData(['user-videos', '7'], pagesOf({ 42: 5 }))
    updateFeedResultCount(qc, ['user-videos'], 42, 1)
    expect(likeCountOf(qc, ['user-videos', '7'])).toBe(6)
  })

  it('前缀匹配更新所有同前缀缓存（多用户作品列表一起改）', () => {
    const qc = new QueryClient()
    qc.setQueryData(['user-videos', '7'], pagesOf({ 42: 5 }))
    qc.setQueryData(['user-videos', '8'], pagesOf({ 42: 1 }))
    updateFeedResultCount(qc, ['user-videos'], 42, -1)
    expect(likeCountOf(qc, ['user-videos', '7'])).toBe(4)
    expect(likeCountOf(qc, ['user-videos', '8'])).toBe(0)
  })

  it('只改目标视频，其他条目与无关缓存不动', () => {
    const qc = new QueryClient()
    qc.setQueryData(['feed'], pagesOf({ 42: 5, 43: 8 }))
    qc.setQueryData(['user', '7'], { user_id: 7 })
    updateFeedResultCount(qc, ['feed'], 42, 1)
    expect(likeCountOf(qc, ['feed'], 0)).toBe(6)
    expect(likeCountOf(qc, ['feed'], 1)).toBe(8)
    expect(qc.getQueryData(['user', '7'])).toEqual({ user_id: 7 })
  })

  it('缓存里没有这个 key 时不新建条目（setQueriesData 与 setQueryData 的差别）', () => {
    const qc = new QueryClient()
    updateFeedResultCount(qc, ['feed'], 42, 1)
    expect(qc.getQueryData(['feed'])).toBeUndefined()
  })
})

describe('updateDetailLike', () => {
  it('更新详情缓存的 like_count 与 is_liked', () => {
    const qc = new QueryClient()
    qc.setQueryData(['video', '42'], { like_count: 5, is_liked: false } as unknown as VideoDetail)
    updateDetailLike(qc, 42, 1, true)
    const detail = qc.getQueryData<VideoDetail>(['video', '42'])
    expect(detail?.like_count).toBe(6)
    expect(detail?.is_liked).toBe(true)
  })
})
