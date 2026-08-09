import { Link, useRouter } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { client } from '../../api/client'
import { useAuthStore } from '../../lib/auth-store'
import { Button } from '../ui/button'

function BackendStatus() {
  // 探针：验证「codegen client → Vite 代理 → 后端」链路（footer 移入 header 后保留）
  const { isPending, isError } = useQuery({
    queryKey: ['health', 'ready'],
    queryFn: async () => {
      const res = await client.GET('/health/ready')
      if (!res.response.ok) throw new Error('后端响应异常')
      return res.response
    },
    refetchInterval: 30_000,
    retry: 1,
  })
  if (isPending) return <span className="text-xs text-zinc-600">…</span>
  if (isError) return <span className="text-xs text-red-400">后端离线</span>
  return <span className="text-xs text-emerald-500">●</span>
}

export function Header() {
  const user = useAuthStore((s) => s.user)
  const accessToken = useAuthStore((s) => s.accessToken)
  const clear = useAuthStore((s) => s.clear)
  const queryClient = useQueryClient()
  const router = useRouter()

  async function handleLogout() {
    const { refreshToken } = useAuthStore.getState()
    if (refreshToken) {
      try {
        // 后端幂等：token 无效也视为已登出
        await client.POST('/api/v1/auth/logout', { body: { refresh_token: refreshToken } })
      } catch {
        /* 网络失败不阻塞本地登出 */
      }
    }
    clear()
    queryClient.clear()
    router.navigate({ to: '/' })
  }

  return (
    <header className="sticky top-0 z-40 border-b border-zinc-800/80 bg-zinc-950/80 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-5xl items-center justify-between px-4">
        <Link to="/" className="text-lg font-bold tracking-tight">
          TokMall
        </Link>
        <BackendStatus />
        <nav className="flex items-center gap-1 text-sm">
          <Link to="/" className="rounded-lg px-3 py-2 hover:bg-zinc-800">
            Feed
          </Link>
          {accessToken && (
            <>
              <Link to="/upload" className="rounded-lg px-3 py-2 hover:bg-zinc-800">
                上传
              </Link>
              <Link to="/me" className="flex items-center gap-2 rounded-lg px-3 py-2 hover:bg-zinc-800">
                {user?.avatar_url ? (
                  <img
                    src={user.avatar_url}
                    alt={user.username}
                    className="h-6 w-6 rounded-full object-cover"
                  />
                ) : (
                  <span className="flex h-6 w-6 items-center justify-center rounded-full bg-zinc-700 text-xs">
                    {user?.username?.slice(0, 1).toUpperCase()}
                  </span>
                )}
                <span className="max-w-24 truncate">{user?.username}</span>
              </Link>
              <Button variant="ghost" size="sm" onClick={handleLogout}>
                退出
              </Button>
            </>
          )}
          {!accessToken && (
            <>
              <Link to="/login" className="rounded-lg px-3 py-2 hover:bg-zinc-800">
                登录
              </Link>
              <Link to="/register">
                <Button size="sm">注册</Button>
              </Link>
            </>
          )}
        </nav>
      </div>
    </header>
  )
}
