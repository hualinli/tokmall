import { createFileRoute, Link, redirect } from '@tanstack/react-router'
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Play, Send, Trash2 } from 'lucide-react'
import { client } from '../api/client'
import { apiRequestError } from '../lib/api-error'
import { StatusBadge } from '../features/video/status-badge'
import { formatRelativeTime } from '../features/feed/format'
import { Button } from '../components/ui/button'
import { Card } from '../components/ui/card'
import { useAuthStore } from '../lib/auth-store'
import { CommerceManager } from '../features/product/commerce-manager'

export const Route = createFileRoute('/me')({
  beforeLoad: ({ context }) => {
    if (!context.auth.getState().accessToken) {
      throw redirect({ to: '/login' })
    }
  },
  component: MePage,
})

function MePage() {
  const queryClient = useQueryClient()
  const me = useAuthStore((state) => state.user)

  const sales = useQuery({
    queryKey: ['author-sales', me?.user_id],
    enabled: Boolean(me?.user_id),
    queryFn: async () => {
      const res = await client.GET('/api/v1/authors/{id}/sales', {
        params: { path: { id: me!.user_id } },
      })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
  })

  const { data: profile } = useQuery({
    queryKey: ['me'],
    queryFn: async () => {
      const res = await client.GET('/api/v1/me')
      if (res.error) throw apiRequestError(res)
      return res.data
    },
  })

  const myVideos = useInfiniteQuery({
    queryKey: ['my-videos'],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) => {
      const res = await client.GET('/api/v1/videos/me', {
        params: { query: { limit: 20, cursor: pageParam } },
      })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
  })

  const publish = useMutation({
    mutationFn: async (videoId: number) => {
      const res = await client.POST('/api/v1/videos/{id}/publish', {
        params: { path: { id: videoId } },
      })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
    onSuccess: () => {
      toast.success('已发布，视频进入 Feed')
      queryClient.invalidateQueries({ queryKey: ['my-videos'] })
      queryClient.invalidateQueries({ queryKey: ['feed'] })
    },
    onError: (e) => toast.error(e.message),
  })

  const remove = useMutation({
    mutationFn: async (videoId: number) => {
      const res = await client.DELETE('/api/v1/videos/{id}', { params: { path: { id: videoId } } })
      if (res.error) throw apiRequestError(res)
      return res.response
    },
    onSuccess: () => {
      toast.success('已删除')
      queryClient.invalidateQueries({ queryKey: ['my-videos'] })
      queryClient.invalidateQueries({ queryKey: ['feed'] })
      queryClient.invalidateQueries({ queryKey: ['user-videos'] })
    },
    onError: (e) => toast.error(e.message),
  })

  const items = myVideos.data?.pages.flatMap((p) => p.items) ?? []

  return (
    <div className="mx-auto w-full max-w-3xl px-4 py-8">
      {sales.data && (
        <Card className="mb-6">
          <h2 className="font-semibold">带货数据</h2>
          <div className="mt-3 grid grid-cols-3 gap-3 text-sm">
            <p>支付订单 <strong className="block text-lg">{sales.data.order_count}</strong></p>
            <p>购买人数 <strong className="block text-lg">{sales.data.buyer_count}</strong></p>
            <p>成交毛额 <strong className="block text-lg">¥{(sales.data.gmv / 100).toFixed(2)}</strong></p>
          </div>
          <p className="mt-3 text-sm text-zinc-400">
            已退款 ¥{(sales.data.refunded_amount / 100).toFixed(2)} · 净成交 ¥{((sales.data.gmv - sales.data.refunded_amount) / 100).toFixed(2)}
          </p>
        </Card>
      )}
      <CommerceManager />
      {/* 资料卡 */}
      <Card className="flex items-center gap-4">
        {profile?.avatar_url ? (
          <img
            src={profile.avatar_url}
            alt={profile.username}
            className="h-16 w-16 rounded-full object-cover"
          />
        ) : (
          <span className="flex h-16 w-16 items-center justify-center rounded-full bg-zinc-700 text-2xl font-semibold">
            {profile?.username.slice(0, 1).toUpperCase() ?? '?'}
          </span>
        )}
        <div className="min-w-0">
          <p className="text-lg font-semibold">{profile?.username}</p>
          <p className="truncate text-sm text-zinc-500">{profile?.email}</p>
          {profile?.bio && <p className="mt-0.5 text-sm text-zinc-400">{profile.bio}</p>}
        </div>
        <Link to="/upload" className="ml-auto shrink-0">
          <Button>上传视频</Button>
        </Link>
      </Card>

      {/* 我的视频管理 */}
      <h2 className="mb-3 mt-8 text-sm font-medium text-zinc-400">我的视频</h2>
      <div className="space-y-2">
        {myVideos.isPending && <p className="py-8 text-center text-sm text-zinc-500">加载中…</p>}
        {myVideos.isError && (
          <p className="py-8 text-center text-sm text-red-400">{myVideos.error.message}</p>
        )}
        {!myVideos.isPending && items.length === 0 && (
          <p className="py-8 text-center text-sm text-zinc-500">
            还没有视频，去上传第一个作品吧
          </p>
        )}
        {items.map((v) => {
          if (v.video_id == null) return null
          return (
            <div key={v.video_id} className="flex items-center gap-3 rounded-xl border border-zinc-800 bg-zinc-900/50 p-3">
              {/* 缩略图/占位 */}
              <Link
                to="/video/$id"
                params={{ id: String(v.video_id) }}
                className="relative block h-20 w-28 shrink-0 overflow-hidden rounded-lg bg-zinc-800"
              >
                {v.cover_url ? (
                  <img src={v.cover_url} alt="" className="h-full w-full object-cover" />
                ) : (
                  <span className="absolute inset-0 flex items-center justify-center">
                    <Play className="h-6 w-6 text-zinc-600" />
                  </span>
                )}
              </Link>
              <div className="min-w-0 flex-1">
                <Link
                  to="/video/$id"
                  params={{ id: String(v.video_id) }}
                  className="line-clamp-1 text-sm font-medium hover:underline"
                >
                  {v.title}
                </Link>
                <div className="mt-1 flex items-center gap-2">
                  {v.status && <StatusBadge status={v.status} />}
                  <span className="text-xs text-zinc-600">{formatRelativeTime(v.created_at)}</span>
                </div>
              </div>
              <div className="flex shrink-0 gap-2">
                {v.status === 'PENDING_REVIEW' && (
                  <Button size="sm" onClick={() => publish.mutate(v.video_id!)} disabled={publish.isPending}>
                    <Send className="h-3.5 w-3.5" /> 发布
                  </Button>
                )}
                {v.status !== 'DELETED' && (
                  <Button
                    variant="destructive"
                    size="sm"
                    onClick={() => {
                      if (window.confirm(`确定删除「${v.title}」？删除后无法恢复`)) {
                        remove.mutate(v.video_id!)
                      }
                    }}
                    disabled={remove.isPending}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                )}
              </div>
            </div>
          )
        })}
      </div>

      {myVideos.hasNextPage && (
        <div className="py-4 text-center">
          <Button variant="outline" size="sm" onClick={() => myVideos.fetchNextPage()}>
            加载更多
          </Button>
        </div>
      )}
    </div>
  )
}
