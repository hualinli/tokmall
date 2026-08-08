import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/user/$id')({
  component: UserPage,
})

function UserPage() {
  const { id } = Route.useParams()
  return (
    <div className="py-16 text-center text-zinc-400">
      <p className="text-2xl font-semibold text-zinc-200">用户 {id} 占位</p>
      <p className="mt-2 text-sm">F3/F5 里程碑接入公开资料 + 作品列表 + 关注</p>
    </div>
  )
}
