import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import type { components } from '../api/types'

type AuthResult = components['schemas']['AuthResult']
type PublicUser = components['schemas']['PublicUser']

/**
 * 客户端认证态：token 对 + 当前用户。
 * persist 落 localStorage → 刷新页面恢复登录态。
 * 注意：刷新轮换（rotation）意味着旧 refresh token 单次有效，
 * 多标签页并发刷新会互相黑名单 —— 学习项目接受此限制（见 ADR）。
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
