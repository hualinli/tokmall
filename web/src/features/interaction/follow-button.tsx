import { useEffect, useState } from 'react'
import { useMutation, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'
import { cn } from '../../lib/cn'
import { client } from '../../api/client'
import { apiErrorMessage } from '../../lib/api-error'
import { useAuthStore } from '../../lib/auth-store'
import type { components } from '../../api/types'
import { Button } from '../../components/ui/button'

type PublicProfile = components['schemas']['PublicProfile']

interface FollowButtonProps {
  userId: number
  following: boolean
  isMe: boolean
  size?: 'sm' | 'md'
  className?: string
}

/**
 * 关注按钮（乐观更新）。
 * 与 LikeButton 同样的约束：意图（follow/unfollow）走 variables，
 * 不在 mutationFn 闭包读 state（TanStack Query 异步间隙闭包陷阱）。
 * 禁止自关注：后端返回 400，前端直接隐藏按钮（is_me）。
 */
export function FollowButton({ userId, following, isMe, size = 'md', className }: FollowButtonProps) {
  const accessToken = useAuthStore((s) => s.accessToken)
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const [ui, setUi] = useState(following)

  const mutation = useMutation({
    mutationFn: async (follow: boolean) => {
      const res = follow
        ? await client.POST('/api/v1/users/{id}/follow', { params: { path: { id: userId } } })
        : await client.DELETE('/api/v1/users/{id}/follow', { params: { path: { id: userId } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    onMutate: (follow) => {
      setUi(follow)
      updateProfileCache(queryClient, userId, follow)
      return { prev: following }
    },
    onError: (_err, _follow, context) => {
      setUi(context?.prev ?? following)
      updateProfileCache(queryClient, userId, context?.prev ?? following)
      toast.error('操作失败，请重试')
    },
    onSuccess: (data) => {
      if (data) updateProfileCache(queryClient, userId, data.following === true)
    },
  })

  // 服务端变化回正（mutation 进行中跳过，防覆盖乐观 UI）
  useEffect(() => {
    if (!mutation.isPending) setUi(following)
  }, [following, mutation.isPending])

  if (isMe) return null

  function handleClick() {
    if (!accessToken) {
      const redirect = window.location.pathname + window.location.search
      navigate({ to: '/login', search: { redirect } })
      return
    }
    mutation.mutate(!ui)
  }

  return (
    <Button
      data-testid="follow-button"
      onClick={handleClick}
      disabled={mutation.isPending}
      size={size}
      variant={ui ? 'outline' : 'default'}
      className={cn('min-w-20', className)}
    >
      {ui ? '已关注' : '+ 关注'}
    </Button>
  )
}

/** 同步用户资料缓存（is_following + 关注数 ±1） */
function updateProfileCache(qc: QueryClient, userId: number, following: boolean) {
  qc.setQueryData<PublicProfile>(['user', String(userId)], (old) => {
    if (!old) return old
    return {
      ...old,
      is_following: following,
      following_count: Math.max(0, old.following_count + (following ? 1 : -1)),
    }
  })
}
