import { useEffect, useMemo, useRef, useState, type DragEvent } from 'react'
import { ImagePlus, X } from 'lucide-react'
import { cn } from '../../lib/cn'
import { IMAGE_MAX_SIZE } from './use-upload-flow'

interface CoverPickerProps {
  cover: File | null
  onSelect: (file: File) => void
  onClear: () => void
}

/** 可选封面：图片选择 + 预览（jpg/png/webp，5MB 内） */
export function CoverPicker({ cover, onSelect, onClear }: CoverPickerProps) {
  const inputRef = useRef<HTMLInputElement>(null)
  const [dragging, setDragging] = useState(false)
  // 对象 URL 不能在渲染期创建：每次渲染都会新建一个且旧的永不释放（内存泄漏 + 图片闪烁）。
  // 这里按 File 记忆并在替换/卸载时撤销。
  const preview = useMemo(() => (cover ? URL.createObjectURL(cover) : null), [cover])
  useEffect(() => {
    if (!preview) return
    return () => URL.revokeObjectURL(preview)
  }, [preview])

  function handleFile(f?: File) {
    if (f && f.type.startsWith('image/') && f.size <= IMAGE_MAX_SIZE) {
      onSelect(f)
    }
  }

  return (
    <div>
      <input
        ref={inputRef}
        type="file"
        accept="image/jpeg,image/png,image/webp"
        className="hidden"
        onChange={(e) => handleFile(e.target.files?.[0])}
      />
      {!cover ? (
        <button
          type="button"
          onClick={() => inputRef.current?.click()}
          onDragOver={(e) => {
            e.preventDefault()
            setDragging(true)
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={(e: DragEvent) => {
            e.preventDefault()
            setDragging(false)
            handleFile(e.dataTransfer.files[0])
          }}
          className={cn(
            'flex w-full items-center justify-center gap-2 rounded-xl border border-dashed border-zinc-700 py-3 text-sm text-zinc-500 transition-colors',
            dragging ? 'border-zinc-400 bg-zinc-900 text-zinc-300' : 'hover:border-zinc-500',
          )}
        >
          <ImagePlus className="h-4 w-4" />
          添加封面（可选）· jpg/png/webp
        </button>
      ) : (
        <div className="flex items-center gap-3">
          <div className="relative h-20 w-32 shrink-0 overflow-hidden rounded-lg bg-zinc-800">
            {preview && <img src={preview} alt="封面预览" className="h-full w-full object-cover" />}
          </div>
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm">{cover.name}</p>
            <p className="text-xs text-zinc-500">{(cover.size / 1024).toFixed(0)} KB</p>
            <button
              type="button"
              onClick={onClear}
              className="mt-1 text-xs text-zinc-400 hover:text-zinc-200"
            >
              移除封面
            </button>
          </div>
          <button
            type="button"
            onClick={onClear}
            className="rounded-full p-1.5 text-zinc-500 hover:bg-zinc-800 hover:text-zinc-200"
            aria-label="移除封面"
          >
            <X className="h-4 w-4" />
          </button>
        </div>
      )}
    </div>
  )
}
