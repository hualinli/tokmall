import type { components } from '../api/types'
import { useAuthStore } from './auth-store'

type PublicUser = components['schemas']['PublicUser']

/** 跨标签页广播的登录态快照（纯数据，不含 action） */
interface AuthSnapshot {
  accessToken: string | null
  refreshToken: string | null
  user: PublicUser | null
}

const CHANNEL_NAME = 'tokmall-auth-sync'
/** 与 zustand persist 的存储键一致，storage 事件靠它识别登录态更新 */
const STORAGE_KEY = 'tokmall-auth'

/**
 * 快照指纹：值相同就不再应用、不再广播。
 * 两个标签页都监听 storage 事件时，无脑回写会造成「A 写 → B 收到 → B 写 → A 收到」的
 * 无限回声（每次 setState 都会触发 persist 写盘），指纹是断环的关键。
 */
function fingerprint(s: AuthSnapshot): string {
  return JSON.stringify([s.accessToken, s.refreshToken, s.user?.user_id ?? null, s.user?.username ?? null])
}

function snapshotOf(): AuthSnapshot {
  const { accessToken, refreshToken, user } = useAuthStore.getState()
  return { accessToken, refreshToken, user }
}

function parsePersisted(raw: string | null): AuthSnapshot | null {
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as { state?: Partial<AuthSnapshot> }
    const state = parsed.state
    if (!state) return null
    return {
      accessToken: state.accessToken ?? null,
      refreshToken: state.refreshToken ?? null,
      user: state.user ?? null,
    }
  } catch {
    // 存储被外部写坏：当作没有登录态，不做任何事（下次登录会覆盖）
    return null
  }
}

/**
 * 多标签页登录态同步。
 *
 * 为什么必须做：refresh token 是「单次有效 + 轮换」的（见 docs/architecture-decisions.md#auth），
 * 多个标签页同时使用旧 token 刷新时，只有一个请求能 Claim 成功。
 * 轮换成功会广播新 token 对，减少其他标签页继续使用旧凭证；
 * 广播不提供刷新互斥，无法阻止广播到达前的并发刷新竞争。
 *
 * 通道选择：优先 BroadcastChannel（同源标签页/Worker，低延迟、不落盘）；
 * 老浏览器回退到 storage 事件（zustand persist 本来就会把登录态写 localStorage，
 * 其他标签页监听 key 变化即可，代价是要解析 JSON）。
 *
 * 安全提示：token 存 localStorage 意味着 XSS 可窃取凭证；换成 HttpOnly Cookie 需要
 * 后端配合 CSRF 防护，属于本期范围外（已在 auth-store 注释中显式记录该权衡）。
 */
export function initAuthSync(): void {
  if (typeof window === 'undefined') return
  let lastFingerprint = fingerprint(snapshotOf())

  function apply(next: AuthSnapshot) {
    const nextFingerprint = fingerprint(next)
    if (nextFingerprint === lastFingerprint) return
    lastFingerprint = nextFingerprint
    // 直接 setState：走 setAuth/clear 会再次触发下面的 subscribe，造成广播回声
    useAuthStore.setState(next)
  }

  const channel = typeof BroadcastChannel === 'undefined' ? null : new BroadcastChannel(CHANNEL_NAME)
  channel?.addEventListener('message', (event: MessageEvent<AuthSnapshot>) => {
    apply(event.data)
  })

  window.addEventListener('storage', (event) => {
    if (event.key !== STORAGE_KEY) return
    const next = parsePersisted(event.newValue)
    if (next) apply(next)
  })

  useAuthStore.subscribe(() => {
    const snapshot = snapshotOf()
    const nextFingerprint = fingerprint(snapshot)
    if (nextFingerprint === lastFingerprint) return
    lastFingerprint = nextFingerprint
    // 没有 BroadcastChannel 时不必手动写盘：persist 已经把 state 写进 localStorage，
    // 其他标签页会收到 storage 事件（即回退路径）。
    channel?.postMessage(snapshot)
  })
}
