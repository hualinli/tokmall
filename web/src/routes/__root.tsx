import { createRootRouteWithContext, Outlet } from '@tanstack/react-router'
import { Header } from '../components/layout/header'
import { AuthLifecycle } from '../lib/auth-lifecycle'
import { useAuthStore } from '../lib/auth-store'

// 路由守卫（beforeLoad）能读到的 router context 类型：
// 必须通过 createRootRouteWithContext 声明，子路由才能推断出 context.auth
export const Route = createRootRouteWithContext<{
  auth: typeof useAuthStore
}>()({
  component: RootLayout,
})

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
