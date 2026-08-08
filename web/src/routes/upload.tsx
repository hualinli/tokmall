import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/upload')({
  component: UploadPage,
})

function UploadPage() {
  return (
    <div className="py-16 text-center text-zinc-400">
      <p className="text-2xl font-semibold text-zinc-200">上传占位</p>
      <p className="mt-2 text-sm">F4 里程碑接入三步流水线（票证 → PUT → 建记录）</p>
    </div>
  )
}
