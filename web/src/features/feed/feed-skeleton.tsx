/** 首屏加载骨架：整屏闪烁占位 */
export function FeedSkeleton() {
  return (
    <div className="h-full space-y-px">
      {[0, 1].map((i) => (
        <div key={i} className="relative h-full animate-pulse bg-zinc-900">
          <div className="absolute inset-x-0 bottom-0 space-y-3 p-6">
            <div className="h-5 w-2/3 rounded bg-zinc-800" />
            <div className="h-4 w-1/3 rounded bg-zinc-800" />
          </div>
        </div>
      ))}
    </div>
  )
}
