import { useEffect, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'
import { Heart } from 'lucide-react'
import { cn } from '../../lib/cn'
import { client } from '../../api/client'
import { apiErrorMessage } from '../../lib/api-error'
import { useAuthStore } from '../../lib/auth-store'
import { useLikedStore } from '../../lib/liked-store'
import { formatCount } from '../feed/format'
import { updateDetailLike, updateFeedResultCount } from './like-cache'

interface LikeButtonProps {
  videoId: number
  count: number
  /** 服务端 is_liked（详情页有；Feed 无 → 走 session store） */
  liked?: boolean
  size?: 'sm' | 'lg'
  className?: string
}

/**
 * 点赞按钮（乐观更新）。
 *
 * 关键实现约束：**意图（like 还是 unlike）必须走 mutation 的 variables 传递**，
 * 绝不能在 mutationFn/onMutate 闭包里读 state 决定方向——
 * TanStack Query 的 mutate() 在 onMutate 与 mutationFn 之间有异步间隙，
 * onMutate 里 setState 触发的重渲染会让 mutationFn 拿到翻转后的新闭包，
 * 导致"想 POST 却发出 DELETE"。本组件踩过这个坑（详见 F3 review）。
 *
 * 乐观流：onMutate 翻转 UI + 同步 Query 缓存 → 失败回滚 → 成功写 session store。
 * 幂等后端保证方向正确，onSuccess 无需再改缓存（避免双倍计数）。
 */
export function LikeButton({ videoId, count, liked = false, size = 'sm', className }: LikeButtonProps) {
  const accessToken = useAuthStore((s) => s.accessToken)
  const storeLiked = useLikedStore((s) => s.liked[videoId])
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  // 服务端无 is_liked 时用 session store 补；两者都没有 → false
  const baseLiked = storeLiked ?? liked
  const [ui, setUi] = useState({ count, liked: baseLiked })

  const mutation = useMutation({
    // variables: like=true 表示「点成已赞」
    mutationFn: async (like: boolean) => {
      const res = like
        ? await client.POST('/api/v1/videos/{id}/like', { params: { path: { id: videoId } } })
        : await client.DELETE('/api/v1/videos/{id}/like', { params: { path: { id: videoId } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    onMutate: (like) => {
      const delta = like ? 1 : -1
      setUi((p) => ({ count: Math.max(0, p.count + delta), liked: like }))
      updateFeedResultCount(queryClient, ['feed'], videoId, delta)
      updateFeedResultCount(queryClient, ['user-videos'], videoId, delta)
      updateDetailLike(queryClient, videoId, delta, like)
      return { prevCount: ui.count, prevLiked: baseLiked, delta }
    },
    onError: (_err, _like, context) => {
      if (context) {
        setUi({ count: context.prevCount, liked: context.prevLiked })
        updateFeedResultCount(queryClient, ['feed'], videoId, -context.delta)
        updateFeedResultCount(queryClient, ['user-videos'], videoId, -context.delta)
        updateDetailLike(queryClient, videoId, -context.delta, context.prevLiked)
      }
      toast.error('操作失败，请重试')
    },
    onSuccess: (data) => {
      if (data) useLikedStore.getState().set(videoId, data.liked === true)
    },
  })

  // 服务端数据变化（其他端点赞/取消）时回正本地乐观态；
  // 注意：mutation 进行中必须跳过，否则会覆盖乐观 UI。
  useEffect(() => {
    if (!mutation.isPending) {
      setUi({ count, liked: baseLiked })
    }
  }, [count, baseLiked, mutation.isPending])

  function handleClick() {
    if (!accessToken) {
      const redirect = window.location.pathname + window.location.search
      navigate({ to: '/login', search: { redirect } })
      return
    }
    mutation.mutate(!ui.liked)
  }

  return (
    <button
      type="button"
      data-testid="like-button"
      onClick={handleClick}
      disabled={mutation.isPending}
      className={cn(
        'group flex flex-col items-center gap-0.5 transition-transform active:scale-90',
        className,
      )}
      aria-label={ui.liked ? '取消点赞' : '点赞'}
    >
      <Heart
        className={cn(
          size === 'lg' ? 'h-9 w-9' : 'h-7 w-7',
          'drop-shadow transition-colors',
          ui.liked ? 'fill-red-500 text-red-500' : 'group-hover:text-red-400',
        )}
      />
      <span className="text-xs">{formatCount(Math.max(ui.count, 0))}</span>
    </button>
  )
}
