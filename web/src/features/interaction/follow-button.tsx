import { useEffect, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'
import { cn } from '../../lib/cn'
import { client } from '../../api/client'
import { apiRequestError } from '../../lib/api-error'
import { useAuthStore } from '../../lib/auth-store'
import { Button } from '../../components/ui/button'
import { updateFollowCache } from './follow-cache'

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
      if (res.error) throw apiRequestError(res)
      return res.data
    },
    onMutate: (follow) => {
      setUi(follow)
      updateFollowCache(queryClient, userId, follow)
      return { prev: following }
    },
    onError: (_err, _follow, context) => {
      setUi(context?.prev ?? following)
      updateFollowCache(queryClient, userId, context?.prev ?? following)
      toast.error('操作失败，请重试')
    },
    onSuccess: (data, follow) => {
      // 以服务端返回为准做纠偏；更新器按值幂等，与 onMutate 重复应用也不会重复计数
      updateFollowCache(queryClient, userId, data?.following ?? follow)
      // 关注关系变了 → 关注 Feed 内容失效（下次进入重新拉）
      queryClient.invalidateQueries({ queryKey: ['following-feed'] })
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
