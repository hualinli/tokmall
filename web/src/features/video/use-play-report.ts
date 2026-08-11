import { useEffect, useRef } from 'react'
import { client } from '../../api/client'
import { useAuthStore } from '../../lib/auth-store'

/**
 * 播放事件上报（每视频会话一次）：
 * - 时机：组件实例首次 enabled（FeedCard 激活播放 / 详情页挂载）时上报
 * - event_id：crypto.randomUUID()，服务端按唯一约束幂等去重（网络重试不重复计数）
 * - 幂等：ref 锁保证同一实例只报一次，防 autoplay 恢复、切声、重复激活多次计数
 * - 未登录不报（play 是受保护端点）；上报失败静默——播放链路不能依赖上报
 */
export function usePlayReport(videoId: number, enabled: boolean) {
  const accessToken = useAuthStore((s) => s.accessToken)
  const reported = useRef(false)

  useEffect(() => {
    if (!enabled || !accessToken || reported.current) return
    reported.current = true // 先锁再上报：防 React StrictMode 双跑
    client
      .POST('/api/v1/videos/{id}/play', {
        params: { path: { id: videoId } },
        body: { event_id: crypto.randomUUID() },
      })
      .catch(() => {
        /* 上报失败不阻塞播放，计数缺失可接受（V1 语义） */
      })
  }, [videoId, enabled, accessToken])
}
