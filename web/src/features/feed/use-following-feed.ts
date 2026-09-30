import { useInfiniteQuery } from '@tanstack/react-query'
import { client } from '../../api/client'
import { apiErrorMessage } from '../../lib/api-error'
import { FEED_PAGE_SIZE } from './use-feed'

/**
 * 关注 Feed 无限查询（需登录）：
 * - 端点 /api/v1/feed/following：拉模式，全部关注作者视频按发布时间排序
 * - enabled 控制：未登录时禁用（后端 401 无意义）
 * - 不依赖 URL cursor：个性化内容从第一页开始，翻页状态在 query 内存
 */
export function useFollowingFeedQuery(enabled: boolean) {
  return useInfiniteQuery({
    queryKey: ['following-feed'],
    enabled,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) => {
      const res = await client.GET('/api/v1/feed/following', {
        params: {
          query: { limit: FEED_PAGE_SIZE, cursor: pageParam ?? undefined },
        },
      })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
  })
}
