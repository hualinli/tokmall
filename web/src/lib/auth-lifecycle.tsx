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
    // 抖动 13~14min：多标签页同时在线时，别让各自的定时器在同一毫秒轮换
    // （rotation 是单次消费，撞车会互相把 token 拉黑；auth-refresh 有兜底重试，
    // 这里只是把撞车概率再压低）。定时器在 token 变化时会重建（跨标签同步也会触发）。
    const delay = 13 * 60_000 + Math.floor(Math.random() * 60_000)
    const timer = setInterval(() => {
      refreshAccessToken().catch(() => {
        /* 已由 doRefresh 清空登录态 */
      })
    }, delay)
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
