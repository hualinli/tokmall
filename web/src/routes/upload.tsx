import { createFileRoute, Link, redirect } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { CheckCircle2, Send } from 'lucide-react'
import { Dropzone } from '../features/upload/upload-dropzone'
import { useUploadFlow, type UploadStage } from '../features/upload/use-upload-flow'
import { Button } from '../components/ui/button'
import { Card, CardTitle, Field } from '../components/ui/card'
import { Input } from '../components/ui/input'

export const Route = createFileRoute('/upload')({
  beforeLoad: ({ context }) => {
    if (!context.auth.getState().accessToken) {
      throw redirect({ to: '/login' })
    }
  },
  component: UploadPage,
})

const STAGE_LABEL: Record<UploadStage, string> = {
  idle: '',
  requesting: '正在申请上传凭证…',
  uploading: '正在上传…',
  creating: '正在创建视频记录…',
  done: '',
  error: '',
}

function UploadPage() {
  const queryClient = useQueryClient()
  const flow = useUploadFlow()
  const [file, setFile] = useState<File | null>(null)
  const [title, setTitle] = useState('')

  const canStart =
    !!file && !!title.trim() && (flow.stage === 'idle' || flow.stage === 'error')

  async function handleStart() {
    if (!file || !title.trim()) return
    const ok = await flow.start(file, title.trim())
    if (ok) {
      queryClient.invalidateQueries({ queryKey: ['my-videos'] })
      toast.success('上传成功，待发布')
    }
  }

  return (
    <div className="mx-auto w-full max-w-xl px-4 py-10">
      <Card>
        <CardTitle>上传视频</CardTitle>
        <div className="mt-6 space-y-5">
          <Dropzone
            file={file}
            onSelect={setFile}
            onClear={() => {
              setFile(null)
              flow.reset()
            }}
            error={flow.stage === 'error' ? flow.error : null}
          />

          <Field label="标题" hint="200 字以内">
            <Input
              value={title}
              maxLength={200}
              placeholder="给视频起个标题…"
              onChange={(e) => setTitle(e.target.value)}
              disabled={flow.stage === 'uploading' || flow.stage === 'creating'}
            />
          </Field>

          {/* 进度区 */}
          {(flow.stage === 'requesting' ||
            flow.stage === 'uploading' ||
            flow.stage === 'creating') && (
            <div>
              <div className="flex items-center justify-between text-sm">
                <span className="text-zinc-400">{STAGE_LABEL[flow.stage]}</span>
                {flow.stage === 'uploading' && (
                  <span className="font-mono text-zinc-300">{flow.progress}%</span>
                )}
              </div>
              <div className="mt-2 h-2 overflow-hidden rounded-full bg-zinc-800">
                <div
                  className="h-full rounded-full bg-emerald-500 transition-all duration-200"
                  style={{ width: `${flow.stage === 'uploading' ? flow.progress : flow.stage === 'requesting' || flow.stage === 'creating' ? 30 : 0}%` }}
                />
              </div>
            </div>
          )}

          {/* 成功态 */}
          {flow.stage === 'done' && flow.result && (
            <div className="flex items-center gap-3 rounded-xl border border-emerald-500/30 bg-emerald-500/10 p-4">
              <CheckCircle2 className="h-8 w-8 shrink-0 text-emerald-400" />
              <div className="min-w-0">
                <p className="text-sm font-medium text-emerald-300">视频已上传，待发布</p>
                <p className="text-xs text-emerald-500/80">
                  发布后才会出现在公共 Feed 中
                </p>
              </div>
            </div>
          )}

          {/* 操作区 */}
          {flow.stage !== 'done' ? (
            <Button
              size="lg"
              className="w-full"
              disabled={!canStart || flow.stage === 'uploading' || flow.stage === 'creating'}
              onClick={handleStart}
            >
              {flow.stage === 'error' ? '重试上传' : '上传'}
            </Button>
          ) : (
            <div className="flex gap-3">
              <Link to="/video/$id" params={{ id: String(flow.result!.videoId) }} className="flex-1">
                <Button size="lg" className="w-full">
                  查看视频
                </Button>
              </Link>
              <Link to="/me" className="flex-1">
                <Button size="lg" variant="outline" className="w-full">
                  <Send className="h-4 w-4" /> 去发布
                </Button>
              </Link>
            </div>
          )}
        </div>
      </Card>
    </div>
  )
}
