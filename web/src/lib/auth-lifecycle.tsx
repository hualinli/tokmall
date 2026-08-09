import { useEffect } from 'react'
import { useRouter, useRouterState } from '@tanstack/react-router'
import { useAuthStore } from './auth-store'
import { refreshAccessToken } from './auth-refresh'

/**
 * 认证生命周期（挂在根布局，不渲染任何 UI）：
 * 1. 预刷新：access 15min，每 14min 静默轮换一次，避免请求途中过期。
 *    刷新失败会由 doRefresh 清空登录态，这里静默吞掉（下方跳转逻辑接管）。
 * 2. 被登出（手动登出 / 刷新失败）→ 若在受保护页，跳登录。
 */
export function AuthLifecycle() {
  const accessToken = useAuthStore((s) => s.accessToken)
  const refreshToken = useAuthStore((s) => s.refreshToken)
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const router = useRouter()

  useEffect(() => {
    if (!accessToken || !refreshToken) return
    const timer = setInterval(() => {
      refreshAccessToken().catch(() => {
        /* 已由 doRefresh 清空登录态 */
      })
    }, 14 * 60_000)
    return () => clearInterval(timer)
  }, [accessToken, refreshToken])

  const isProtected = pathname.startsWith('/me') || pathname.startsWith('/upload')
  useEffect(() => {
    if (!accessToken && isProtected) {
      router.navigate({ to: '/login' })
    }
  }, [accessToken, isProtected, router])

  return null
}
