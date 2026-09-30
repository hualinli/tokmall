import { useCallback, useEffect, useRef } from 'react'

/**
 * 哨兵元素：滚动到容器底部附近（提前 200px）触发回调。
 * 用于无限滚动的 fetchNextPage 触发。
 *
 * 返回「回调 ref」而不是 RefObject，两个原因：
 * 1. 哨兵元素是列表加载完后才挂载的（加载中是骨架屏），用 mount-only effect 会
 *    在 ref.current 还是 null 时就退出，永远观察不到元素——无限滚动直接失效；
 *    回调 ref 在元素真正挂载/卸载时被调用，天然对上生命周期。
 * 2. 回调本身存进 ref：调用方每次渲染传入的都是新箭头函数，若作为 effect 依赖，
 *    IntersectionObserver 会被反复销毁重建（滚动中尤其浪费，重建瞬间还会丢交叉事件）。
 */
export function useSentinel(onVisible: () => void): (el: HTMLDivElement | null) => void {
  const callbackRef = useRef(onVisible)
  callbackRef.current = onVisible
  const observerRef = useRef<IntersectionObserver | null>(null)

  // 卸载兜底：正常情况下回调 ref 会先收到 null，这里再断一次（幂等）
  useEffect(
    () => () => {
      observerRef.current?.disconnect()
      observerRef.current = null
    },
    [],
  )

  return useCallback((el: HTMLDivElement | null) => {
    observerRef.current?.disconnect()
    observerRef.current = null
    if (!el) return
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries[0].isIntersecting) callbackRef.current()
      },
      { rootMargin: '200px 0px' },
    )
    obs.observe(el)
    observerRef.current = obs
  }, [])
}
