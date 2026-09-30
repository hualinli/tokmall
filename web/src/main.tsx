import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Toaster } from 'sonner'
import { routeTree } from './routeTree.gen'
import { useAuthStore } from './lib/auth-store'
import { initAuthSync } from './lib/auth-sync'
import { shouldRetryQuery } from './lib/query-retry'
import './index.css'

// 多标签页登录态同步（轮换广播 / 登出广播）：必须在首个请求之前装上
initAuthSync()

const router = createRouter({
  routeTree,
  // 路由守卫依赖的外部状态（zustand store 实例）
  context: { auth: useAuthStore },
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 5_000,
      refetchOnWindowFocus: false,
      // 4xx（404/409/401/429）不重试：确定性拒绝重试只会放大请求、拖慢报错
      retry: shouldRetryQuery,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster theme="dark" position="top-center" richColors />
    </QueryClientProvider>
  </StrictMode>,
)
