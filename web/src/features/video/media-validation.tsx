import type { components } from '../../api/types'

type Validation = components['schemas']['MediaValidation']

const labels: Record<Validation['status'], string> = {
  PENDING: '校验中，请稍候',
  PASSED: '校验通过',
  REJECTED: '校验未通过',
  SKIPPED: '未进行媒体校验',
}

export function MediaValidation({ validation }: { validation?: Validation }) {
  if (!validation) return null
  return (
    <p role="status" className={`mt-2 text-xs ${validation.status === 'REJECTED' ? 'text-red-400' : 'text-zinc-400'}`}>
      {labels[validation.status]}
      {validation.status === 'SKIPPED' && validation.can_publish && '，当前允许发布'}
      {validation.reason && <span className="block mt-1">{validation.reason}</span>}
    </p>
  )
}
