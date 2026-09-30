import { useInfiniteQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Play } from 'lucide-react'
import { client } from '../../api/client'
import { apiRequestError } from '../../lib/api-error'
import { useSentinel } from '../feed/use-sentinel'
import { formatCount, formatDuration } from '../feed/format'

const WORKS_PAGE_SIZE = 12

/**
 * 用户主页作品列表：公开已发布视频（FeedItem 结构），游标无限滚动。
 */
export function UserWorks({ userId }: { userId: number }) {
  const works = useInfiniteQuery({
    queryKey: ['user-videos', String(userId)],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) => {
      const res = await client.GET('/api/v1/users/{id}/videos', {
        params: {
          path: { id: userId },
          query: { limit: WORKS_PAGE_SIZE, cursor: pageParam },
        },
      })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
  })

  const items = works.data?.pages.flatMap((p) => p.items) ?? []
  const sentinelRef = useSentinel(() => {
    if (works.hasNextPage && !works.isFetchingNextPage) works.fetchNextPage()
  })

  if (works.isPending) {
    return (
      <div className="grid grid-cols-3 gap-1.5">
        {Array.from({ length: 6 }).map((_, i) => (
          <div key={i} className="aspect-[3/4] animate-pulse rounded-lg bg-zinc-800" />
        ))}
      </div>
    )
  }

  if (works.isError) {
    return <p className="py-8 text-center text-sm text-red-400">{works.error.message}</p>
  }

  if (items.length === 0) {
    return <p className="py-12 text-center text-sm text-zinc-500">还没有作品</p>
  }

  return (
    <>
      <div className="grid grid-cols-3 gap-1.5">
        {items.map((it) => (
          <Link
            key={it.video_id}
            to="/video/$id"
            params={{ id: String(it.video_id) }}
            className="group relative block aspect-[3/4] overflow-hidden rounded-lg bg-zinc-900"
          >
            {it.cover_url ? (
              <img
                src={it.cover_url}
                alt={it.title}
                className="h-full w-full object-cover transition-transform group-hover:scale-105"
              />
            ) : (
              <span className="absolute inset-0 flex items-center justify-center">
                <Play className="h-8 w-8 text-zinc-600" />
              </span>
            )}
            <div className="absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/80 to-transparent p-2">
              <p className="line-clamp-1 text-xs">{it.title}</p>
              <p className="mt-0.5 flex items-center gap-1 text-[10px] text-zinc-300">
                <Play className="h-2.5 w-2.5" />
                {formatCount(it.like_count)} · {formatDuration(it.duration_seconds)}
              </p>
            </div>
          </Link>
        ))}
      </div>
      <div ref={sentinelRef} className="h-16" />
      {works.isFetchingNextPage && (
        <p className="py-4 text-center text-sm text-zinc-500">加载中…</p>
      )}
    </>
  )
}
