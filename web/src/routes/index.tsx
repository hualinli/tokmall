import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { z } from 'zod'
import { FeedCard } from '../features/feed/feed-card'
import { FeedSkeleton } from '../features/feed/feed-skeleton'
import { useFeedQuery } from '../features/feed/use-feed'
import { useFollowingFeedQuery } from '../features/feed/use-following-feed'
import { useSentinel } from '../features/feed/use-sentinel'
import { useAuthStore } from '../lib/auth-store'
import { Button } from '../components/ui/button'
import { cn } from '../lib/cn'
import type { components } from '../api/types'

// cursor 进 URL：刷新/分享停留在原时间线位置（replace 同步，不产生历史记录）。
// 只对推荐 tab 生效：关注 tab 个性化且通常从第一页开始，不污染 URL。
const feedSearchSchema = z.object({
  cursor: z.string().optional(),
})

export const Route = createFileRoute('/')({
  validateSearch: feedSearchSchema,
  component: FeedPage,
})

type FeedTab = 'recommend' | 'following'

function FeedPage() {
  const [tab, setTab] = useState<FeedTab>('recommend')
  const accessToken = useAuthStore((s) => s.accessToken)

  return (
    <div className="relative h-[calc(100dvh-3.5rem)]">
      {/* 悬浮 tab：不占文档流，避免压缩滚动容器高度（滚动偏移依赖原布局） */}
      <div className="absolute left-0 right-0 top-0 z-10 flex items-center justify-center gap-1 bg-gradient-to-b from-zinc-950/80 to-transparent py-3 pb-6">
        {(
          [
            ['recommend', '为你推荐'],
            ['following', '关注'],
          ] as [FeedTab, string][]
        ).map(([key, label]) => (
          <button
            key={key}
            type="button"
            onClick={() => setTab(key)}
            className={cn(
              'rounded-full px-4 py-1.5 text-sm font-medium transition-colors',
              tab === key ? 'bg-zinc-100 text-zinc-900' : 'text-zinc-400 hover:text-zinc-200',
            )}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === 'recommend' ? (
        <RecommendTimeline />
      ) : accessToken ? (
        <FollowingTimeline />
      ) : (
        <div className="flex h-full flex-col items-center justify-center gap-3 text-zinc-500">
          <p className="text-lg">登录后查看关注动态</p>
          <Button variant="outline" onClick={() => window.location.assign('/login')}>
            去登录
          </Button>
        </div>
      )}
    </div>
  )
}

/** 推荐 tab：与历史行为一致，cursor 同步 URL（刷新/分享停留原位置）。 */
function RecommendTimeline() {
  const { cursor } = Route.useSearch()
  const navigate = useNavigate({ from: Route.fullPath })
  const feed = useFeedQuery(cursor)

  // 每取到新页 → 把最新 cursor 同步进 URL（replace 不产生历史）
  const lastCursor = feed.data?.pages.at(-1)?.next_cursor
  useEffect(() => {
    if (lastCursor && lastCursor !== cursor) {
      navigate({ search: { cursor: lastCursor }, replace: true })
    }
  }, [lastCursor, cursor, navigate])

  return (
    <FeedTimeline
      items={feed.data?.pages.flatMap((p) => p.items) ?? []}
      isPending={feed.isPending}
      isError={feed.isError}
      errorMessage={feed.error?.message}
      hasNextPage={feed.hasNextPage}
      isFetchingNextPage={feed.isFetchingNextPage}
      onLoadMore={() => feed.fetchNextPage()}
      onRetry={() => feed.refetch()}
      emptyHint="还没有视频，去发布第一个作品吧"
    />
  )
}

/** 关注 tab：个性化内容，cursor 不写 URL；切走再切回从第一页开始。 */
function FollowingTimeline() {
  const feed = useFollowingFeedQuery(true)
  return (
    <FeedTimeline
      items={feed.data?.pages.flatMap((p) => p.items) ?? []}
      isPending={feed.isPending}
      isError={feed.isError}
      errorMessage={feed.error?.message}
      hasNextPage={feed.hasNextPage}
      isFetchingNextPage={feed.isFetchingNextPage}
      onLoadMore={() => feed.fetchNextPage()}
      onRetry={() => feed.refetch()}
      emptyHint="关注作者后，这里会出现他们的新作品"
    />
  )
}

interface TimelineProps {
  items: components['schemas']['FeedItem'][]
  isPending: boolean
  isError: boolean
  errorMessage?: string
  hasNextPage: boolean
  isFetchingNextPage: boolean
  onLoadMore: () => void
  onRetry: () => void
  emptyHint: string
}

/**
 * 整屏滚动时间线（推荐/关注共用）：
 * - IntersectionObserver 哨兵到底加载下一页
 * - 首屏数据到达 → 默认激活第一条
 */
function FeedTimeline({
  items,
  isPending,
  isError,
  errorMessage,
  hasNextPage,
  isFetchingNextPage,
  onLoadMore,
  onRetry,
  emptyHint,
}: TimelineProps) {
  const [activeId, setActiveId] = useState<number | null>(null)

  useEffect(() => {
    if (activeId === null && items[0]) {
      setActiveId(items[0].video_id)
    }
  }, [items, activeId])

  const sentinelRef = useSentinel(() => {
    if (hasNextPage && !isFetchingNextPage) {
      onLoadMore()
    }
  })

  if (isPending) {
    return (
      <div className="h-full overflow-hidden">
        <FeedSkeleton />
      </div>
    )
  }

  if (isError) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-4">
        <p className="max-w-md text-center text-sm text-zinc-400">{errorMessage}</p>
        <Button onClick={onRetry}>重试</Button>
      </div>
    )
  }

  if (items.length === 0) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 text-zinc-500">
        <p className="text-xl">{emptyHint}</p>
      </div>
    )
  }

  return (
    <div className="h-full snap-y snap-mandatory overflow-y-auto">
      {items.map((item) => (
        <FeedCard
          key={item.video_id}
          item={item}
          active={item.video_id === activeId}
          onActivate={(id) => setActiveId(id)}
          onRetry={onRetry}
        />
      ))}
      <div ref={sentinelRef} className="h-24 snap-none" />
      {isFetchingNextPage && (
        <div className="flex h-24 snap-none items-center justify-center text-sm text-zinc-500">
          加载中…
        </div>
      )}
      {!hasNextPage && (
        <div className="flex h-24 snap-none items-center justify-center text-sm text-zinc-600">到底啦</div>
      )}
    </div>
  )
}
