import { createFileRoute, Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ArrowLeft, Trash2 } from 'lucide-react'
import { client } from '../api/client'
import { apiErrorMessage } from '../lib/api-error'
import { useAuthStore } from '../lib/auth-store'
import { VideoPlayer } from '../features/video/video-player'
import { StatusBadge } from '../features/video/status-badge'
import { LikeButton } from '../features/interaction/like-button'
import { formatRelativeTime } from '../features/feed/format'
import { Button } from '../components/ui/button'

export const Route = createFileRoute('/video/$id')({
  component: VideoPage,
})

function VideoPage() {
  const { id } = Route.useParams()
  const videoId = Number(id)
  const me = useAuthStore((s) => s.user)
  const queryClient = useQueryClient()

  const { data, isPending, error } = useQuery({
    queryKey: ['video', id],
    queryFn: async () => {
      const res = await client.GET('/api/v1/videos/{id}', { params: { path: { id: videoId } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
  })

  const publish = useMutation({
    mutationFn: async () => {
      const res = await client.POST('/api/v1/videos/{id}/publish', {
        params: { path: { id: videoId } },
      })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    onSuccess: () => {
      toast.success('已发布')
      queryClient.invalidateQueries({ queryKey: ['video', id] })
      queryClient.invalidateQueries({ queryKey: ['my-videos'] })
      queryClient.invalidateQueries({ queryKey: ['feed'] })
    },
    onError: (e) => toast.error(e.message),
  })

  const remove = useMutation({
    mutationFn: async () => {
      const res = await client.DELETE('/api/v1/videos/{id}', { params: { path: { id: videoId } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.response
    },
    onSuccess: () => {
      toast.success('已删除')
      queryClient.invalidateQueries({ queryKey: ['feed'] })
      queryClient.invalidateQueries({ queryKey: ['my-videos'] })
      queryClient.invalidateQueries({ queryKey: ['user-videos'] })
    },
    onError: (e) => toast.error(e.message),
  })

  if (isPending) {
    return (
      <div className="mx-auto w-full max-w-2xl py-12">
        <div className="aspect-video animate-pulse rounded-xl bg-zinc-900" />
        <div className="mt-6 h-6 w-2/3 animate-pulse rounded bg-zinc-800" />
      </div>
    )
  }

  if (error || !data) {
    return (
      <div className="mx-auto flex w-full max-w-2xl flex-col items-center gap-4 py-20 text-center">
        <p className="text-zinc-400">{error?.message ?? '视频不存在'}</p>
        <p className="text-sm text-zinc-600">可能已删除或未发布</p>
        <Link to="/">
          <Button variant="outline">
            <ArrowLeft className="h-4 w-4" /> 回到 Feed
          </Button>
        </Link>
      </div>
    )
  }

  const isMine = me?.user_id === data.author_id

  return (
    <div className="mx-auto w-full max-w-2xl px-4 py-8">
      <VideoPlayer src={data.video_url} />

      <div className="mt-5 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold">{data.title}</h1>
          <Link
            to="/user/$id"
            params={{ id: String(data.author_id) }}
            className="mt-2 flex items-center gap-2 hover:opacity-80"
          >
            {data.author.avatar_url ? (
              <img
                src={data.author.avatar_url}
                alt={data.author.username}
                className="h-8 w-8 rounded-full object-cover"
              />
            ) : (
              <span className="flex h-8 w-8 items-center justify-center rounded-full bg-zinc-700 text-sm font-semibold">
                {data.author.username.slice(0, 1).toUpperCase()}
              </span>
            )}
            <span className="text-sm text-zinc-300">@{data.author.username}</span>
          </Link>
        </div>
        <div className="flex flex-col items-center gap-2">
          <LikeButton videoId={videoId} count={data.like_count} liked={data.is_liked} size="lg" />
          {isMine && data.status !== 'DELETED' && (
            <Button variant="destructive" size="sm" onClick={() => remove.mutate()}>
              <Trash2 className="h-4 w-4" />
            </Button>
          )}
        </div>
      </div>

      <div className="mt-4 flex flex-wrap items-center gap-2 text-xs text-zinc-500">
        {isMine && <StatusBadge status={data.status} />}
        {data.published_at && <span>{formatRelativeTime(data.published_at)}发布</span>}
        <span>创建于 {formatRelativeTime(data.created_at)}</span>
        {isMine && data.status === 'PENDING_REVIEW' && (
          <Button size="sm" onClick={() => publish.mutate()} disabled={publish.isPending}>
            发布
          </Button>
        )}
      </div>

      {remove.isPending && <p className="mt-2 text-sm text-zinc-500">删除中…</p>}
    </div>
  )
}
