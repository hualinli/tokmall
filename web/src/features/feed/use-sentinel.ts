import { useEffect, useRef, type RefObject } from 'react'

/**
 * 哨兵元素：滚动到容器底部附近（提前 200px）触发回调。
 * 用于无限滚动的 fetchNextPage 触发。
 */
export function useSentinel(onVisible: () => void): RefObject<HTMLDivElement | null> {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries[0].isIntersecting) onVisible()
      },
      { rootMargin: '200px 0px' },
    )
    obs.observe(el)
    return () => obs.disconnect()
  }, [onVisible])

  return ref
}
