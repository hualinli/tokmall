import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/')({
  component: FeedPage,
})

function FeedPage() {
  return (
    <div className="py-8 text-center text-zinc-400">
      <p className="text-2xl font-semibold text-zinc-200">Feed 占位</p>
      <p className="mt-2 text-sm">F2 里程碑接入游标分页 + 静音自动播放</p>
    </div>
  )
}
