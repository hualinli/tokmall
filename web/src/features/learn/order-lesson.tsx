import { useEffect, useState } from 'react'
import {
  emptyProgress,
  experiments,
  outcomes,
  parseProgress,
  progressKey,
  questions,
  tasks,
  type LessonProgress,
} from './order-lesson-data'

const button =
  'rounded-lg border border-zinc-700 px-3 py-2 text-sm hover:bg-zinc-800 disabled:cursor-not-allowed disabled:opacity-40'
const panel =
  'space-y-4 rounded-xl border border-zinc-800 bg-zinc-900/40 p-4 sm:p-6'

function Experiment({ onExplore }: { onExplore: (id: string) => void }) {
  const [selected, setSelected] = useState(0)
  const [prediction, setPrediction] = useState('')
  const [step, setStep] = useState(-1)
  const experiment = experiments[selected]
  const frame = step >= 0 ? experiment.frames[step] : undefined
  const finished = step === experiment.frames.length - 1

  function advance() {
    const next = step + 1
    setStep(next)
    if (next === experiment.frames.length - 1) onExplore(experiment.id)
  }

  return (
    <section className={panel} aria-labelledby="order-lab-title">
      <div>
        <p className="text-xs text-emerald-400">01 · 先预测，再推演</p>
        <h2 id="order-lab-title" className="mt-1 text-lg font-medium">
          同一次购买，五种走向
        </h2>
        <p className="mt-2 text-sm leading-6 text-zinc-400">
          每个场景独立重置数据。这是按源码编写的步骤模拟，不调用下单接口，也不实际执行数据库事务。
        </p>
      </div>
      <div className="flex flex-wrap gap-2" aria-label="选择下单场景">
        {experiments.map((item, i) => (
          <button
            key={item.id}
            type="button"
            aria-pressed={selected === i}
            className={`${button} ${selected === i ? 'border-emerald-500 bg-emerald-950 text-emerald-200' : ''}`}
            onClick={() => {
              setSelected(i)
              setStep(-1)
              setPrediction('')
            }}
          >
            {item.title}
          </button>
        ))}
      </div>
      <p className="text-sm leading-6 text-zinc-300">
        起点：{experiment.setup}
      </p>
      <p className="break-words rounded-lg bg-zinc-950 p-3 font-mono text-xs leading-6">
        请求：{experiment.request}
      </p>
      <fieldset disabled={step >= 0} className="space-y-2">
        <legend className="mb-2 text-sm">你预计会得到什么结果？</legend>
        <div className="flex flex-wrap gap-2">
          {outcomes.map((item) => (
            <label
              key={item.id}
              className={`${button} flex items-center gap-2 ${prediction === item.id ? 'border-emerald-500' : ''}`}
            >
              <input
                type="radio"
                name="order-prediction"
                value={item.id}
                checked={prediction === item.id}
                onChange={() => setPrediction(item.id)}
              />
              {item.label}
            </label>
          ))}
        </div>
      </fieldset>
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          className={button}
          disabled={!prediction || finished}
          onClick={advance}
        >
          {step < 0 ? '开始推演' : '下一步'}
        </button>
        <button
          type="button"
          className={button}
          disabled={step < 0}
          onClick={() => {
            setStep(-1)
            setPrediction('')
          }}
        >
          重新预测
        </button>
        <span className="text-xs text-zinc-500">
          步骤 {step + 1} / {experiment.frames.length}
        </span>
      </div>
      <div
        className="min-h-28 rounded-lg border border-zinc-700 bg-zinc-950 p-4"
        role="status"
        aria-live="polite"
      >
        {frame ? (
          <>
            <h3 className="font-medium text-emerald-300">{frame[0]}</h3>
            <p className="mt-2 text-sm leading-6 text-zinc-300">{frame[1]}</p>
          </>
        ) : (
          <p className="text-sm text-zinc-500">
            先选一个预测，再逐步查看锁、写入和提交的变化。
          </p>
        )}
      </div>
      {finished && (
        <div className="space-y-2 border-t border-zinc-700 pt-4" role="status">
          <p className="text-sm font-medium text-emerald-300">
            {prediction === experiment.answer ? '预测正确' : '预测需要调整'} ·{' '}
            {outcomes.find((o) => o.id === experiment.answer)?.label}
          </p>
          <p className="text-sm leading-6 text-zinc-200">
            提交后的事实：{experiment.result}
          </p>
          <p className="text-sm leading-6 text-zinc-400">{experiment.why}</p>
        </div>
      )}
    </section>
  )
}

const reading = [
  [
    'HTTP 入口',
    'internal/order/handler.go',
    '认证身份与请求 DTO 如何组成 CreateParams？领域冲突最终如何映射到响应？',
  ],
  [
    '业务语义',
    'internal/order/service.go · Create / requestFingerprint',
    'key 会去空格；指纹包含用户、SKU、数量、来源和券，价格由服务端读取。',
  ],
  [
    '事务边界',
    'internal/order/repository.go · Create',
    '同键锁 → 查原单并比指纹 → 读 SKU / 锁券 → 订单、明细、流水、核销券、超时事件 → 条件扣库存 → 提交。',
  ],
  [
    '数据库兜底',
    'migrations/000012_create_order_foundation.up.sql',
    '查找 orders_user_idempotency_key；再沿库存相关迁移检查非负约束。不要把 service 预检当作最终并发防线。',
  ],
]

export function OrderLesson() {
  const [progress, setProgress] = useState<LessonProgress>(() => {
    try {
      return parseProgress(window.localStorage.getItem(progressKey))
    } catch {
      return emptyProgress()
    }
  })
  const [saved, setSaved] = useState(true)
  const [experimentVersion, setExperimentVersion] = useState(0)
  useEffect(() => {
    try {
      window.localStorage.setItem(progressKey, JSON.stringify(progress))
      setSaved(true)
    } catch {
      setSaved(false)
    }
  }, [progress])

  const correct = questions.filter(
    (q, i) => progress.answers[i] === q.answer,
  ).length
  const done = progress.explored.length + correct + progress.tasks.length
  const total = experiments.length + questions.length + tasks.length

  return (
    <div className="space-y-6" aria-label="下单与幂等实战样章">
      <section className="space-y-4 rounded-xl border border-emerald-900 bg-emerald-950/20 p-4 sm:p-6">
        <p className="text-xs font-medium text-emerald-400">
          实战样章 · 下单与幂等
        </p>
        <h2 className="text-xl font-semibold">
          把“会重试”讲成一条有证据的交易链路
        </h2>
        <p className="text-sm leading-6 text-zinc-300">
          目标：分清重试与新购，解释同键锁和指纹的分工，并证明库存失败时没有留下半张订单。建议先推演，再读下面的源码场景，最后完成练习。
        </p>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <label htmlFor="order-learning-progress" className="text-sm">
            学习进度 {done} / {total}
          </label>
          <button
            type="button"
            className="text-xs text-zinc-400 underline underline-offset-4"
            onClick={() => {
              setProgress(emptyProgress())
              setExperimentVersion((v) => v + 1)
            }}
          >
            重置本章进度和笔记
          </button>
        </div>
        <progress
          id="order-learning-progress"
          value={done}
          max={total}
          className="h-2 w-full accent-emerald-500"
        />
        <p className="text-xs leading-5 text-zinc-400" role="status">
          {saved
            ? '进度和笔记保存在当前浏览器；清除网站数据后会丢失。'
            : '浏览器存储不可用，当前进度仅在本次页面停留期间保留。'}{' '}
          实验 {progress.explored.length}/{experiments.length} · 理解检查{' '}
          {correct}/{questions.length} · 自评任务 {progress.tasks.length}/
          {tasks.length}。进度不代表自动验证了你的代码。
        </p>
      </section>

      <Experiment
        key={experimentVersion}
        onExplore={(id) =>
          setProgress((p) =>
            p.explored.includes(id)
              ? p
              : { ...p, explored: [...p.explored, id] },
          )
        }
      />

      <section className={panel} aria-labelledby="order-reading-title">
        <p className="text-xs text-emerald-400">02 · 沿源码找证据</p>
        <h2 id="order-reading-title" className="text-lg font-medium">
          一次请求，四个落点
        </h2>
        <ol className="space-y-4">
          {reading.map(([label, file, description]) => (
            <li key={label} className="border-l-2 border-emerald-900 pl-4">
              <h3 className="text-sm font-medium">{label}</h3>
              <p className="mt-1 break-all font-mono text-xs text-emerald-300">
                {file}
              </p>
              <p className="mt-1 text-sm leading-6 text-zinc-400">
                {description}
              </p>
            </li>
          ))}
        </ol>
        <p className="text-xs leading-5 text-zinc-500">
          路径供你在本地编辑器定位。下方原有场景提供关键片段；完整实现和迁移是事实入口。
        </p>
      </section>

      <section className={panel} aria-labelledby="order-task-title">
        <p className="text-xs text-emerald-400">03 · 亲手完成</p>
        <h2 id="order-task-title" className="text-lg font-medium">
          让项目留下你的修改和判断
        </h2>
        {tasks.map((task, i) => (
          <div
            key={task.title}
            className="space-y-3 border-t border-zinc-800 pt-4"
          >
            <h3 className="text-sm font-medium">{task.title}</h3>
            <p className="text-sm leading-6 text-zinc-300">
              {task.instruction}
            </p>
            <details className="text-sm text-zinc-400">
              <summary className="cursor-pointer py-1 text-emerald-300">
                展开提示
              </summary>
              <p className="mt-2 leading-6">{task.hint}</p>
            </details>
            <details className="text-sm text-zinc-400">
              <summary className="cursor-pointer py-1 text-emerald-300">
                展开参考思路
              </summary>
              <pre className="mt-2 overflow-x-auto rounded-lg bg-zinc-950 p-3 text-xs leading-6 whitespace-pre-wrap">
                <code>{task.reference}</code>
              </pre>
            </details>
            <p className="text-sm leading-6 text-zinc-400">
              验收：{task.acceptance}
            </p>
            <label className="flex items-start gap-2 text-sm text-zinc-300">
              <input
                type="checkbox"
                className="mt-1 accent-emerald-500"
                checked={progress.tasks.includes(i)}
                onChange={(event) => {
                  const checked = event.target.checked
                  setProgress((p) => ({
                    ...p,
                    tasks: checked
                      ? [...p.tasks, i]
                      : p.tasks.filter((n) => n !== i),
                  }))
                }}
              />
              我已按上述标准完成并记录验证范围
            </label>
          </div>
        ))}
      </section>

      <section className={panel} aria-labelledby="order-quiz-title">
        <p className="text-xs text-emerald-400">04 · 检查理解</p>
        <h2 id="order-quiz-title" className="text-lg font-medium">
          能否接住这三个追问？
        </h2>
        {questions.map((q, i) => (
          <fieldset
            key={q.title}
            className="space-y-2 border-t border-zinc-800 pt-4"
          >
            <legend className="pr-2 text-sm font-medium">
              {i + 1}. {q.title}
            </legend>
            {q.options.map((option, n) => (
              <label
                key={option}
                className="flex items-start gap-2 rounded-lg bg-zinc-950 p-3 text-sm text-zinc-300"
              >
                <input
                  type="radio"
                  className="mt-1 accent-emerald-500"
                  name={`order-question-${i}`}
                  checked={progress.answers[i] === n}
                  onChange={() =>
                    setProgress((p) => ({
                      ...p,
                      answers: { ...p.answers, [i]: n },
                    }))
                  }
                />
                {option}
              </label>
            ))}
            {progress.answers[i] !== undefined && (
              <p role="status" className="text-sm leading-6 text-emerald-300">
                {progress.answers[i] === q.answer ? '回答正确。' : '再想一想。'}
                {q.explanation}
              </p>
            )}
          </fieldset>
        ))}
      </section>

      <section className={panel} aria-labelledby="order-notes-title">
        <p className="text-xs text-emerald-400">05 · 整理成自己的项目经历</p>
        <h2 id="order-notes-title" className="text-lg font-medium">
          用你做过的事，支撑你的介绍
        </h2>
        <p className="text-sm leading-6 text-zinc-400">
          试着用 90 秒说明：业务问题 → 实现与取舍 → 亲手修改 → 验证证据 →
          已知边界。写清你完成了什么；阅读过的实现与自己做过的修改分别描述。
        </p>
        <label htmlFor="order-evidence" className="block text-sm">
          我的修改与验证记录
        </label>
        <textarea
          id="order-evidence"
          rows={8}
          maxLength={8000}
          value={progress.notes}
          onChange={(event) => {
            const notes = event.target.value
            setProgress((p) => ({ ...p, notes }))
          }}
          className="w-full rounded-lg border border-zinc-700 bg-zinc-950 p-3 text-sm leading-6 focus:border-emerald-500 focus:outline-none"
          placeholder={
            '我修改了：\n这样设计的原因与代价：\n我运行的测试和结果（含环境/跳过项）：\n代码或实验记录位置：\n当前仍不能保证：'
          }
        />
        <p className="text-xs text-zinc-500">
          {progress.notes.length} / 8000 字符 ·
          可直接选择文字复制；本页不会自动生成性能或生产经历。
        </p>
        <details className="text-sm text-zinc-400">
          <summary className="cursor-pointer text-emerald-300">
            查看讲述骨架
          </summary>
          <p className="mt-2 leading-6">
            项目学习了下单重试与库存一致性。我亲手完成了［具体修改］，通过［真实运行的测试］验证［行为］。同键锁负责串行化，指纹校验业务事实，唯一约束兜底；订单、库存、券与超时事件在
            PG
            本地事务中提交。代价是热点行竞争与共享数据库边界，目前支付仍为模拟。
          </p>
        </details>
      </section>
    </div>
  )
}
