import { createRootRouteWithContext, Outlet, useRouter } from '@tanstack/react-router'
import { Header } from '../components/layout/header'
import { AuthLifecycle } from '../lib/auth-lifecycle'
import { useAuthStore } from '../lib/auth-store'
import { Button } from '../components/ui/button'

// 路由守卫（beforeLoad）能读到的 router context 类型：
// 必须通过 createRootRouteWithContext 声明，子路由才能推断出 context.auth
export const Route = createRootRouteWithContext<{
  auth: typeof useAuthStore
}>()({
  component: RootLayout,
  // 渲染错误兜底：路由组件抛错时不白屏
  errorComponent: RootError,
  notFoundComponent: RootNotFound,
})

function RootError({ error, reset }: { error: Error; reset: () => void }) {
  return (
    <div className="flex h-[calc(100dvh-3.5rem)] flex-col items-center justify-center gap-4 px-4 text-center">
      <p className="text-xl font-semibold">页面出错了</p>
      <p className="max-w-md text-sm text-zinc-500">{error.message}</p>
      <Button onClick={reset}>重试</Button>
    </div>
  )
}

function RootNotFound() {
  const router = useRouter()
  return (
    <div className="flex h-[calc(100dvh-3.5rem)] flex-col items-center justify-center gap-4 text-center">
      <p className="text-5xl font-bold text-zinc-700">404</p>
      <p className="text-zinc-400">页面不存在</p>
      <Button variant="outline" onClick={() => router.navigate({ to: '/' })}>
        回到 Feed
      </Button>
    </div>
  )
}

function RootLayout() {
  return (
    <div className="flex min-h-dvh flex-col bg-zinc-950 text-zinc-100">
      <AuthLifecycle />
      <Header />
      {/* 不做 max-width 约束：Feed 需要全屏；各页面自行控制内容宽度 */}
      <main className="w-full flex-1">
        <Outlet />
      </main>
    </div>
  )
}
