import { useRef, useState, type DragEvent } from 'react'
import { Film, X } from 'lucide-react'
import { cn } from '../../lib/cn'

interface DropzoneProps {
  file: File | null
  onSelect: (file: File) => void
  onClear: () => void
  error: string | null
}

/** 拖拽/点击选择视频文件 */
export function Dropzone({ file, onSelect, onClear, error }: DropzoneProps) {
  const inputRef = useRef<HTMLInputElement>(null)
  const [dragging, setDragging] = useState(false)

  function handleFile(f?: File) {
    if (f && f.type.startsWith('video/')) {
      onSelect(f)
    }
  }

  function onDrop(e: DragEvent) {
    e.preventDefault()
    setDragging(false)
    handleFile(e.dataTransfer.files[0])
  }

  return (
    <div>
      <input
        ref={inputRef}
        type="file"
        accept="video/mp4,video/webm,video/quicktime"
        className="hidden"
        onChange={(e) => handleFile(e.target.files?.[0])}
      />
      {!file ? (
        <button
          type="button"
          onClick={() => inputRef.current?.click()}
          onDragOver={(e) => {
            e.preventDefault()
            setDragging(true)
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
          className={cn(
            'flex w-full flex-col items-center justify-center gap-3 rounded-2xl border-2 border-dashed py-16 transition-colors',
            dragging ? 'border-zinc-400 bg-zinc-900' : 'border-zinc-700 hover:border-zinc-500',
          )}
        >
          <Film className="h-10 w-10 text-zinc-500" />
          <p className="text-sm text-zinc-400">
            点击选择或拖拽视频到此处
          </p>
          <p className="text-xs text-zinc-600">mp4 / webm / mov · 1MB - 512MB</p>
        </button>
      ) : (
        <div className="flex items-center gap-3 rounded-2xl border border-zinc-700 bg-zinc-900/60 p-4">
          <Film className="h-8 w-8 shrink-0 text-zinc-400" />
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-medium">{file.name}</p>
            <p className="text-xs text-zinc-500">
              {(file.size / 1024 / 1024).toFixed(1)} MB · {file.type || '未知类型'}
            </p>
          </div>
          <button
            type="button"
            onClick={onClear}
            className="rounded-full p-1.5 text-zinc-500 hover:bg-zinc-800 hover:text-zinc-200"
            aria-label="移除文件"
          >
            <X className="h-4 w-4" />
          </button>
        </div>
      )}
      {error && <p className="mt-2 text-sm text-red-400">{error}</p>}
    </div>
  )
}
