import { createRootRoute, Outlet, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { client } from '../api/client'

export const Route = createRootRoute({
  component: RootLayout,
})

function BackendStatus() {
  // F0 探针：验证「codegen client → Vite 代理 → 后端」整条链路通
  const { isPending, isError } = useQuery({
    queryKey: ['health', 'ready'],
    queryFn: async () => {
      // 后端实际返回 204 空 body（契约声明 200 JSON，存在漂移）：
      // 只依赖 HTTP 状态码判活，不解析 body
      const res = await client.GET('/health/ready')
      if (!res.response.ok) throw new Error('后端响应异常')
      return res.response
    },
    refetchInterval: 30_000,
    retry: 1,
  })

  if (isPending) return <span className="text-xs text-zinc-500">后端探测中…</span>
  if (isError) return <span className="text-xs text-red-400">后端离线</span>
  return (
    <span className="text-xs text-emerald-400">
      后端在线
    </span>
  )
}

function RootLayout() {
  return (
    <div className="flex min-h-dvh flex-col bg-zinc-950 text-zinc-100">
      <header className="sticky top-0 z-40 border-b border-zinc-800/80 bg-zinc-950/80 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-5xl items-center justify-between px-4">
          <Link to="/" className="text-lg font-bold tracking-tight">
            TokMall
          </Link>
          <nav className="flex items-center gap-4 text-sm">
            <Link to="/" className="hover:text-zinc-300">
              Feed
            </Link>
            <Link to="/upload" className="hover:text-zinc-300">
              上传
            </Link>
            <Link to="/me" className="hover:text-zinc-300">
              我的
            </Link>
            <Link to="/login" className="hover:text-zinc-300">
              登录
            </Link>
          </nav>
          <BackendStatus />
        </div>
      </header>
      <main className="mx-auto w-full max-w-5xl flex-1">
        <Outlet />
      </main>
    </div>
  )
}
