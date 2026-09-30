import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import type { components } from '../api/types'

type AuthResult = components['schemas']['AuthResult']
type PublicUser = components['schemas']['PublicUser']

/**
 * 客户端认证态：token 对 + 当前用户。
 * persist 落 localStorage → 刷新页面恢复登录态。
 *
 * 安全权衡：localStorage 里的 token 可被 XSS 读取（HttpOnly Cookie 能防住，
 * 但需要后端配合 CSRF 防护与跨站凭证策略，本期范围外），本项目按学习项目接受该风险。
 *
 * 多标签页：refresh token 单次有效（rotation），每个标签页各自刷新必然互相拉黑。
 * 解决方式见 auth-sync.ts（BroadcastChannel + storage 事件广播新 token 对 / 登出），
 * 由 main.tsx 在启动时调用 initAuthSync() 装上。
 */
interface AuthState {
  accessToken: string | null
  refreshToken: string | null
  user: PublicUser | null
  setAuth: (result: AuthResult) => void
  setUser: (user: PublicUser) => void
  clear: () => void
}

export const useAuthStore = create<AuthState>()(
  persist(
    (set) => ({
      accessToken: null,
      refreshToken: null,
      user: null,
      setAuth: (result) =>
        set({
          accessToken: result.access_token,
          refreshToken: result.refresh_token,
          user: result.user,
        }),
      setUser: (user) => set({ user }),
      clear: () => set({ accessToken: null, refreshToken: null, user: null }),
    }),
    {
      name: 'tokmall-auth',
      // 只持久化数据，函数不入库
      partialize: (s) => ({
        accessToken: s.accessToken,
        refreshToken: s.refreshToken,
        user: s.user,
      }),
    },
  ),
)
