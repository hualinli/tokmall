import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Play, RefreshCw, Volume2, VolumeX } from 'lucide-react'
import type { components } from '../../api/types'
import { LikeButton } from '../interaction/like-button'
import { formatDuration } from './format'

type FeedItem = components['schemas']['FeedItem']

interface FeedCardProps {
  item: FeedItem
  active: boolean
  onActivate: (videoId: number) => void
  onRetry: () => void
}

/**
 * 短视频卡片（整屏一卡，scroll-snap 对齐）：
 * - IntersectionObserver 60% 可见 → 成为活跃视频
 * - 活跃才 play()，其余 pause() + preload=metadata（不浪费解码）
 * - 静音自动播放（浏览器策略）；可手动开声（部分浏览器可能拦截带声播放）
 * - 预签名 URL 过期（2h）→ onError 遮罩 + 重试（refetch 整页拿新 URL）
 */
export function FeedCard({ item, active, onActivate, onRetry }: FeedCardProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const videoRef = useRef<HTMLVideoElement>(null)
  const [muted, setMuted] = useState(true)
  const [playing, setPlaying] = useState(false)
  const [failed, setFailed] = useState(false)

  // 可见性 → 活跃（回调引用稳定，observer 只挂一次）
  const onActivateRef = useRef(onActivate)
  onActivateRef.current = onActivate
  useEffect(() => {
    const el = containerRef.current
    if (!el) return
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries[0].isIntersecting) onActivateRef.current(item.video_id)
      },
      { threshold: 0.6 },
    )
    obs.observe(el)
    return () => obs.disconnect()
  }, [item.video_id])

  // 活跃切换 → play/pause
  useEffect(() => {
    const v = videoRef.current
    if (!v) return
    if (active) {
      // 自动播放失败（少见）静默处理，中心播放按钮兜底
      v.play().catch(() => {})
    } else {
      v.pause()
      setPlaying(false)
    }
  }, [active])

  function toggleMute() {
    const v = videoRef.current
    if (!v) return
    const next = !muted
    setMuted(next)
    v.muted = next
    if (!next) {
      v.play().catch(() => setMuted(true)) // 带声播放被拦截 → 退回静音
    }
  }

  function manualPlay() {
    const v = videoRef.current
    if (!v) return
    v.play().catch(() => {})
  }

  return (
    <div ref={containerRef} className="relative h-full snap-start overflow-hidden">
      <video
        ref={videoRef}
        src={item.video_url}
        muted={muted}
        playsInline
        loop
        preload={active ? 'auto' : 'metadata'}
        className="absolute inset-0 h-full w-full bg-black object-contain"
        onPlay={() => setPlaying(true)}
        onPause={() => setPlaying(false)}
        onError={() => setFailed(true)}
      />

      {/* 底部信息遮罩 */}
      <div className="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/80 via-black/30 to-transparent px-6 pb-8 pt-20">
        <p className="line-clamp-2 text-lg font-semibold drop-shadow">{item.title}</p>
        <div className="mt-3 flex items-center gap-2">
          <Link
            to="/user/$id"
            params={{ id: String(item.author.user_id) }}
            className="pointer-events-auto flex items-center gap-2 rounded-lg hover:opacity-80"
          >
            {item.author.avatar_url ? (
              <img
                src={item.author.avatar_url}
                alt={item.author.username}
                className="h-7 w-7 rounded-full object-cover"
              />
            ) : (
              <span className="flex h-7 w-7 items-center justify-center rounded-full bg-zinc-600 text-xs font-semibold">
                {item.author.username.slice(0, 1).toUpperCase()}
              </span>
            )}
            <span className="text-sm text-zinc-200">@{item.author.username}</span>
          </Link>
        </div>
      </div>

      {/* 右侧操作列 */}
      <div className="absolute bottom-20 right-4 flex flex-col items-center gap-5">
        <LikeButton videoId={item.video_id} count={item.like_count} />
        <button
          type="button"
          onClick={toggleMute}
          className="rounded-full bg-black/40 p-2 backdrop-blur transition-colors hover:bg-black/60"
          aria-label={muted ? '开启声音' : '静音'}
        >
          {muted ? <VolumeX className="h-6 w-6" /> : <Volume2 className="h-6 w-6" />}
        </button>
      </div>

      {/* 时长角标 */}
      <span className="absolute bottom-6 right-4 rounded bg-black/60 px-1.5 py-0.5 text-xs">
        {formatDuration(item.duration_seconds)}
      </span>

      {/* 活跃但未在播放 → 中心播放按钮（自动播放被拦截时兜底） */}
      {active && !playing && !failed && (
        <button
          type="button"
          onClick={manualPlay}
          className="absolute inset-0 flex items-center justify-center"
          aria-label="播放"
        >
          <span className="rounded-full bg-black/40 p-4 backdrop-blur">
            <Play className="h-10 w-10" />
          </span>
        </button>
      )}

      {/* 预签名 URL 过期/加载失败 → 重试（refetch 拿新 URL） */}
      {failed && (
        <button
          type="button"
          onClick={() => {
            setFailed(false)
            onRetry()
          }}
          className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-black/60"
        >
          <RefreshCw className="h-8 w-8" />
          <span className="text-sm text-zinc-300">视频加载失败，点击重试</span>
        </button>
      )}
    </div>
  )
}
