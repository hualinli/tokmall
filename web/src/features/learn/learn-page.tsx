import { chapters, type ChapterId } from './chapters'
import { scenariosByChapter } from './scenarios'
import { OrderLesson } from './order-lesson'

export function LearnPage({
  chapter,
  onChapter,
}: {
  chapter?: ChapterId
  onChapter: (id: ChapterId) => void
}) {
  const currentId = chapter ?? 'assembly'
  const index = Math.max(
    0,
    chapters.findIndex((item) => item.id === currentId),
  )
  const current = chapters[index]
  const scenarios = scenariosByChapter[current.id]
  const prev = chapters[index - 1]
  const next = chapters[index + 1]

  return (
    <div className="mx-auto grid w-full max-w-6xl gap-8 px-4 py-8 lg:grid-cols-[200px_minmax(0,1fr)]">
      <nav aria-label="教程目录" className="lg:sticky lg:top-20 lg:self-start">
        <button type="button" onClick={() => onChapter('order')}
          className="mb-4 w-full rounded-lg border border-emerald-900 bg-emerald-950/30 px-3 py-3 text-left text-sm text-emerald-300">
          从实战样章开始 →
          <span className="mt-1 block text-xs text-zinc-400">下单与幂等 · 推演 / 练习 / 自测</span>
        </button>
        <label className="mb-3 block text-xs text-zinc-500 lg:hidden" htmlFor="learn-chapter">
          章节
        </label>
        <select
          id="learn-chapter"
          className="w-full rounded-lg border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm lg:hidden"
          value={current.id}
          onChange={(event) => onChapter(event.target.value as ChapterId)}
        >
          {chapters.map((item, itemIndex) => (
            <option key={item.id} value={item.id}>
              {itemIndex + 1}. {item.nav}
            </option>
          ))}
        </select>
        <ol className="hidden space-y-1 lg:block">
          {chapters.map((item, itemIndex) => {
            const active = item.id === current.id
            return (
              <li key={item.id}>
                <button
                  type="button"
                  onClick={() => onChapter(item.id)}
                  className={
                    active
                      ? 'w-full rounded-lg bg-zinc-100 px-3 py-2 text-left text-sm text-zinc-900'
                      : 'w-full rounded-lg px-3 py-2 text-left text-sm text-zinc-400 hover:bg-zinc-900 hover:text-zinc-100'
                  }
                  aria-current={active ? 'page' : undefined}
                >
                  <span className="mr-2 font-mono text-xs opacity-60">{itemIndex + 1}</span>
                  {item.nav}
                </button>
              </li>
            )
          })}
        </ol>
      </nav>

      <article className="min-w-0 space-y-8">
        <header className="space-y-2">
          <p className="font-mono text-xs text-zinc-500">
            {index + 1} / {chapters.length}
          </p>
          <h1 className="text-2xl font-semibold tracking-tight">{current.title}</h1>
          <p className="max-w-2xl text-sm leading-6 text-zinc-400">{current.lead}</p>
        </header>

        {current.id === 'order' && <OrderLesson />}

        <section className="rounded-xl border border-zinc-800 bg-zinc-900/40 p-4" aria-label="交互演示">
          {current.scene}
        </section>

        <div className="grid gap-6 md:grid-cols-2">
          <section className="space-y-2">
            <h2 className="text-sm font-medium text-zinc-100">基础知识</h2>
            <p className="text-sm leading-6 text-zinc-300">{current.foundation}</p>
          </section>
          <section className="space-y-2">
            <h2 className="text-sm font-medium text-zinc-100">在 TokMall 里</h2>
            <p className="text-sm leading-6 text-zinc-300">{current.apply}</p>
          </section>
        </div>

        <section className="space-y-8" aria-label="场景讲解">
          <div className="space-y-3">
            <h2 className="text-sm font-medium text-zinc-100">这一节的场景 · {scenarios.length}</h2>
            <ol className="flex flex-wrap gap-2">
              {scenarios.map((item, itemIndex) => (
                <li key={item.id}>
                  <a
                    href={`#${current.id}-${item.id}`}
                    className="inline-block rounded-full border border-zinc-700 px-3 py-1 text-sm text-zinc-300 hover:bg-zinc-800"
                  >
                    {itemIndex + 1}. {item.title}
                  </a>
                </li>
              ))}
            </ol>
          </div>
          {scenarios.map((item, itemIndex) => (
            <section key={item.id} id={`${current.id}-${item.id}`} className="scroll-mt-20 space-y-3 border-t border-zinc-800 pt-6">
              <h3 className="text-base font-medium text-zinc-100">
                {itemIndex + 1}. {item.title}
              </h3>
              <div className="grid gap-4 md:grid-cols-2">
                <div className="space-y-1">
                  <h4 className="text-xs text-zinc-500">基础知识</h4>
                  <p className="text-sm leading-6 text-zinc-300">{item.foundation}</p>
                </div>
                <div className="space-y-1">
                  <h4 className="text-xs text-zinc-500">在 TokMall 里</h4>
                  <p className="text-sm leading-6 text-zinc-300">{item.apply}</p>
                </div>
              </div>
              <p className="font-mono text-xs text-zinc-500">
                {item.code.file} · {item.code.lines}
              </p>
              <pre className="overflow-x-auto rounded-lg border border-zinc-800 bg-zinc-950 p-4 text-xs leading-5 text-zinc-200">
                <code>{item.code.body}</code>
              </pre>
            </section>
          ))}
        </section>

        <div className="flex items-center justify-between gap-3 border-t border-zinc-800 pt-4">
          {prev ? (
            <button
              type="button"
              className="text-sm text-zinc-300 hover:text-zinc-100"
              onClick={() => onChapter(prev.id)}
            >
              上一节 · {prev.nav}
            </button>
          ) : (
            <span />
          )}
          {next ? (
            <button
              type="button"
              className="rounded-lg bg-zinc-100 px-3 py-2 text-sm text-zinc-900"
              onClick={() => onChapter(next.id)}
            >
              下一节 · {next.nav}
            </button>
          ) : (
            <span className="text-sm text-zinc-500">{chapters.length} 节到这里</span>
          )}
        </div>
      </article>
    </div>
  )
}
