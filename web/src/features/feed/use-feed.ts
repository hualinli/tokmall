import { useInfiniteQuery } from '@tanstack/react-query'
import { client } from '../../api/client'
import { apiErrorMessage } from '../../lib/api-error'

export const FEED_PAGE_SIZE = 10

/**
 * Feed 无限查询：
 * - pageParam = 上一页返回的签名 cursor（HMAC，无法伪造）
 * - 首页 URL 带 cursor 时以此为种子 → 刷新/分享停留在原时间线位置
 * - queryKey 固定：URL cursor 用 replace 同步，不能触发重建查询（否则死循环）
 *
 * 预签名 URL 2h 有效：staleTime 短（5s 全局默认），活跃会话内基本不会过期；
 * 过期兜底在 FeedCard 的 onError（refetch 整页拿新 URL）。
 */
export function useFeedQuery(initialCursor: string | undefined) {
  return useInfiniteQuery({
    queryKey: ['feed'],
    initialPageParam: initialCursor,
    queryFn: async ({ pageParam }) => {
      const res = await client.GET('/api/v1/feed', {
        params: {
          query: { limit: FEED_PAGE_SIZE, cursor: pageParam ?? undefined },
        },
      })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  })
}
