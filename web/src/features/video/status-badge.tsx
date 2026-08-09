import type { components } from '../../api/types'
import { cn } from '../../lib/cn'

type VideoStatus = components['schemas']['VideoStatus']

const STATUS_META: Record<VideoStatus, { label: string; className: string }> = {
  UPLOADING: { label: '上传中', className: 'bg-zinc-700 text-zinc-200' },
  PROCESSING: { label: '处理中', className: 'bg-blue-500/20 text-blue-300' },
  PENDING_REVIEW: { label: '待发布', className: 'bg-amber-500/20 text-amber-300' },
  PUBLISHED: { label: '已发布', className: 'bg-emerald-500/20 text-emerald-300' },
  FAILED: { label: '失败', className: 'bg-red-500/20 text-red-300' },
  BLOCKED: { label: '已屏蔽', className: 'bg-red-500/20 text-red-300' },
  DELETED: { label: '已删除', className: 'bg-zinc-800 text-zinc-500' },
}

/** 视频状态徽章（对应后端状态机 7 态） */
export function StatusBadge({ status }: { status: VideoStatus }) {
  const meta = STATUS_META[status] ?? { label: status, className: 'bg-zinc-700 text-zinc-200' }
  return (
    <span className={cn('rounded-full px-2 py-0.5 text-xs font-medium', meta.className)}>
      {meta.label}
    </span>
  )
}
