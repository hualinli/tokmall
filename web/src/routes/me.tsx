import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/me')({
  component: MePage,
})

function MePage() {
  return (
    <div className="py-16 text-center text-zinc-400">
      <p className="text-2xl font-semibold text-zinc-200">我的占位</p>
      <p className="mt-2 text-sm">F3/F5 里程碑接入资料 + 我的视频管理</p>
    </div>
  )
}
