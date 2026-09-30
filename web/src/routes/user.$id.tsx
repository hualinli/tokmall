import { createFileRoute, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ArrowLeft } from 'lucide-react'
import { client } from '../api/client'
import { apiRequestError } from '../lib/api-error'
import { useAuthStore } from '../lib/auth-store'
import { FollowButton } from '../features/interaction/follow-button'
import { UserWorks } from '../features/user/user-works'
import { formatCount } from '../features/feed/format'
import { Button } from '../components/ui/button'

export const Route = createFileRoute('/user/$id')({
  component: UserPage,
})

function UserPage() {
  const { id } = Route.useParams()
  const me = useAuthStore((s) => s.user)

  const { data: profile, isPending, error } = useQuery({
    queryKey: ['user', id],
    queryFn: async () => {
      const res = await client.GET('/api/v1/users/{id}', { params: { path: { id: Number(id) } } })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
  })

  if (isPending) {
    return (
      <div className="mx-auto w-full max-w-3xl px-4 py-12">
        <div className="h-20 animate-pulse rounded-2xl bg-zinc-900" />
      </div>
    )
  }

  if (error || !profile) {
    return (
      <div className="mx-auto flex w-full max-w-3xl flex-col items-center gap-4 px-4 py-20 text-center">
        <p className="text-zinc-400">{error?.message ?? '用户不存在'}</p>
        <Link to="/">
          <Button variant="outline">
            <ArrowLeft className="h-4 w-4" /> 回到 Feed
          </Button>
        </Link>
      </div>
    )
  }

  const isMe = me?.user_id === profile.user_id

  return (
    <div className="mx-auto w-full max-w-3xl px-4 py-8">
      {/* 资料头 */}
      <div className="flex items-center gap-4">
        {profile.avatar_url ? (
          <img
            src={profile.avatar_url}
            alt={profile.username}
            className="h-20 w-20 rounded-full object-cover"
          />
        ) : (
          <span className="flex h-20 w-20 items-center justify-center rounded-full bg-zinc-700 text-3xl font-semibold">
            {profile.username.slice(0, 1).toUpperCase()}
          </span>
        )}
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-3">
            <h1 className="truncate text-2xl font-semibold">{profile.username}</h1>
            {isMe && <span className="text-xs text-zinc-500">（我）</span>}
          </div>
          {profile.bio && <p className="mt-1 line-clamp-2 text-sm text-zinc-400">{profile.bio}</p>}
          <p className="mt-1.5 text-sm text-zinc-500">
            {formatCount(profile.following_count)} 关注 · {formatCount(profile.followers_count)} 粉丝
          </p>
        </div>
        <FollowButton
          userId={profile.user_id}
          following={profile.is_following}
          isMe={isMe}
        />
      </div>

      {/* 作品 */}
      <h2 className="mb-3 mt-8 text-sm font-medium text-zinc-400">作品</h2>
      <UserWorks userId={profile.user_id} />
    </div>
  )
}
