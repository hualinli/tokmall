import { useEffect, useRef, useState } from 'react'
import { Volume2, VolumeX } from 'lucide-react'

/**
 * 详情页播放器：静音自动播放 + 手动开声（与 Feed 相同的浏览器策略处理）。
 * controls 保留给用户拖动进度条/全屏。
 */
export function VideoPlayer({ src, autoPlay = true }: { src: string; autoPlay?: boolean }) {
  const ref = useRef<HTMLVideoElement>(null)
  const [muted, setMuted] = useState(true)

  useEffect(() => {
    if (!autoPlay) return
    ref.current?.play().catch(() => {
      /* 自动播放被拦截：静音按钮可手动触发 */
    })
  }, [autoPlay])

  function toggleMute() {
    const v = ref.current
    if (!v) return
    const next = !muted
    setMuted(next)
    v.muted = next
    if (!next) v.play().catch(() => setMuted(true))
  }

  return (
    <div className="relative">
      <video
        ref={ref}
        src={src}
        muted={muted}
        playsInline
        controls
        preload="auto"
        className="max-h-[70dvh] w-full rounded-xl bg-black object-contain"
      />
      <button
        type="button"
        onClick={toggleMute}
        className="absolute right-3 top-3 rounded-full bg-black/50 p-2 backdrop-blur transition-colors hover:bg-black/70"
        aria-label={muted ? '开启声音' : '静音'}
      >
        {muted ? <VolumeX className="h-5 w-5" /> : <Volume2 className="h-5 w-5" />}
      </button>
    </div>
  )
}
