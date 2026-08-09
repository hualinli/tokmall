import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { z } from 'zod'
import { FeedCard } from '../features/feed/feed-card'
import { FeedSkeleton } from '../features/feed/feed-skeleton'
import { useFeedQuery } from '../features/feed/use-feed'
import { useSentinel } from '../features/feed/use-sentinel'
import { Button } from '../components/ui/button'

// cursor 进 URL：刷新/分享停留在原时间线位置（replace 同步，不产生历史记录）
const feedSearchSchema = z.object({
  cursor: z.string().optional(),
})

export const Route = createFileRoute('/')({
  validateSearch: feedSearchSchema,
  component: FeedPage,
})

function FeedPage() {
  const { cursor } = Route.useSearch()
  // 限定到本路由：navigate 的 search 类型才能精确到 { cursor?: string }
  const navigate = useNavigate({ from: Route.fullPath })

  const feed = useFeedQuery(cursor)
  const items = feed.data?.pages.flatMap((p) => p.items) ?? []
  const [activeId, setActiveId] = useState<number | null>(null)

  // 首屏数据到达 → 默认激活第一条（避免等 observer 才出画）
  useEffect(() => {
    if (activeId === null && items[0]) {
      setActiveId(items[0].video_id)
    }
  }, [items, activeId])

  // 每取到新页 → 把最新 cursor 同步进 URL（replace 不产生历史）
  const lastCursor = feed.data?.pages.at(-1)?.next_cursor
  useEffect(() => {
    if (lastCursor && lastCursor !== cursor) {
      navigate({ search: { cursor: lastCursor }, replace: true })
    }
  }, [lastCursor, cursor, navigate])

  // 滚到底 → 加载下一页
  const sentinelRef = useSentinel(() => {
    if (feed.hasNextPage && !feed.isFetchingNextPage) {
      feed.fetchNextPage()
    }
  })

  function handleRetry() {
    feed.refetch()
  }

  if (feed.isPending) {
    return (
      <div className="h-[calc(100dvh-3.5rem)]">
        <FeedSkeleton />
      </div>
    )
  }

  if (feed.isError) {
    return (
      <div className="flex h-[calc(100dvh-3.5rem)] flex-col items-center justify-center gap-4">
        <p className="max-w-md text-center text-sm text-zinc-400">{feed.error.message}</p>
        <Button onClick={() => feed.refetch()}>重试</Button>
      </div>
    )
  }

  if (items.length === 0) {
    return (
      <div className="flex h-[calc(100dvh-3.5rem)] flex-col items-center justify-center gap-3 text-zinc-500">
        <p className="text-xl">还没有视频</p>
        <p className="text-sm">去发布第一个作品吧</p>
      </div>
    )
  }

  return (
    <div className="h-[calc(100dvh-3.5rem)] snap-y snap-mandatory overflow-y-auto">
      {items.map((item) => (
        <FeedCard
          key={item.video_id}
          item={item}
          active={item.video_id === activeId}
          onActivate={(id) => setActiveId(id)}
          onRetry={handleRetry}
        />
      ))}
      <div ref={sentinelRef} className="h-24 snap-none" />
      {feed.isFetchingNextPage && (
        <div className="flex h-24 snap-none items-center justify-center text-sm text-zinc-500">
          加载中…
        </div>
      )}
      {!feed.hasNextPage && (
        <div className="flex h-24 snap-none items-center justify-center text-sm text-zinc-600">到底啦</div>
      )}
    </div>
  )
}
