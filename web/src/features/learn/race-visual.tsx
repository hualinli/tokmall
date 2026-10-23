import { useEffect, useState } from 'react'
import { cn } from '../../lib/cn'

type Cell = 'idle' | 'hold' | 'wait' | 'gone'

function useStepper(length: number) {
  const [index, setIndex] = useState(0)
  const [playing, setPlaying] = useState(false)

  useEffect(() => {
    if (!playing) return
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setPlaying(false)
      return
    }
    const timer = window.setInterval(() => {
      setIndex((current) => {
        if (current >= length - 1) {
          setPlaying(false)
          return current
        }
        return current + 1
      })
    }, 1300)
    return () => window.clearInterval(timer)
  }, [playing, length])

  function play() {
    if (index >= length - 1) setIndex(0)
    setPlaying(true)
  }

  function reset() {
    setPlaying(false)
    setIndex(0)
  }

  return { index, setIndex, playing, setPlaying, play, reset }
}

function Controls({
  index,
  length,
  playing,
  onPlay,
  onPause,
  onReset,
}: {
  index: number
  length: number
  playing: boolean
  onPlay: () => void
  onPause: () => void
  onReset: () => void
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      {playing ? (
        <button type="button" onClick={onPause} className="rounded-lg bg-zinc-100 px-3 py-1.5 text-sm text-zinc-900">
          暂停
        </button>
      ) : (
        <button type="button" onClick={onPlay} className="rounded-lg bg-zinc-100 px-3 py-1.5 text-sm text-zinc-900">
          {index >= length - 1 ? '重播' : '播放'}
        </button>
      )}
      <button
        type="button"
        onClick={onReset}
        className="rounded-lg border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300"
      >
        回到开头
      </button>
      <span className="font-mono text-xs text-zinc-500">
        {index + 1} / {length}
      </span>
      <span className="flex gap-1" aria-hidden>
        {Array.from({ length }, (_, dot) => (
          <span
            key={dot}
            className={cn(
              'h-1.5 w-1.5 rounded-full transition-colors motion-reduce:transition-none',
              dot === index ? 'bg-zinc-100' : dot < index ? 'bg-zinc-500' : 'bg-zinc-700',
            )}
          />
        ))}
      </span>
    </div>
  )
}

function cellClass(cell: Cell, hot: boolean) {
  if (cell === 'hold') return 'border-emerald-700/80 bg-emerald-950 text-emerald-200'
  if (cell === 'wait') {
    return cn(
      'border-amber-700/80 bg-amber-950 text-amber-100',
      hot && 'motion-safe:animate-pulse',
    )
  }
  if (cell === 'gone') return 'border-zinc-800 bg-zinc-950 text-zinc-600 line-through'
  return 'border-zinc-800 bg-zinc-950 text-zinc-500'
}

function cellLabel(cell: Cell) {
  if (cell === 'hold') return '持有'
  if (cell === 'wait') return '等待'
  if (cell === 'gone') return '已释放'
  return '空闲'
}

const branchFrames = [
  {
    status: 'PAID',
    ship: 'idle' as Cell,
    refund: 'idle' as Cell,
    text: '订单是已支付。发货和申请退款都要改这一行，两条命令从这里分叉。',
  },
  {
    status: 'PAID',
    ship: 'wait' as Cell,
    refund: 'hold' as Cell,
    text: '申请退款先执行 FOR UPDATE。发货堵在同一行上，还没读到新状态。',
  },
  {
    status: 'REFUND_REQUESTED',
    ship: 'wait' as Cell,
    refund: 'hold' as Cell,
    text: '退款提交，状态离开已支付。锁放开之后，发货才能进来。',
  },
  {
    status: 'REFUND_REQUESTED',
    ship: 'hold' as Cell,
    refund: 'gone' as Cell,
    text: '发货拿到锁，读到的已经是退款处理中。履约窄表里没有从这里出发的边。',
  },
  {
    status: 'REFUND_REQUESTED',
    ship: 'gone' as Cell,
    refund: 'gone' as Cell,
    text: '发货失败并回滚自己的修改。订单留在退款中，两条分支只有一条提交成功。',
  },
]

function BranchPlay() {
  const play = useStepper(branchFrames.length)
  const frame = branchFrames[play.index]
  return (
    <div className="space-y-4">
      <Controls
        index={play.index}
        length={branchFrames.length}
        playing={play.playing}
        onPlay={play.play}
        onPause={() => play.setPlaying(false)}
        onReset={play.reset}
      />
      <div className="rounded-lg border border-zinc-800 px-4 py-4">
        <p className="text-xs text-zinc-500">订单行</p>
        <p
          className="mt-1 font-mono text-lg text-zinc-100 transition-colors motion-reduce:transition-none"
          aria-live="polite"
        >
          {frame.status}
        </p>
        <div className="mt-4 grid gap-2 sm:grid-cols-2">
          <div className={cn('rounded-lg border px-3 py-3 text-sm transition-colors motion-reduce:transition-none', cellClass(frame.ship, false))}>
            发货 · {cellLabel(frame.ship)}
          </div>
          <div className={cn('rounded-lg border px-3 py-3 text-sm transition-colors motion-reduce:transition-none', cellClass(frame.refund, false))}>
            申请退款 · {cellLabel(frame.refund)}
          </div>
        </div>
      </div>
      <p className="text-sm leading-6 text-zinc-200">{frame.text}</p>
    </div>
  )
}

const failScripts = {
  stock: [
    { outcome: 'open' as const, marks: ['pending', 'pending', 'pending', 'pending', 'pending'] as const, text: '事务已开始。这些写入还没被别的连接看见。' },
    { outcome: 'open' as const, marks: ['written', 'pending', 'pending', 'pending', 'pending'] as const, text: '订单行写好了，仍包在这个事务里。' },
    { outcome: 'open' as const, marks: ['written', 'written', 'written', 'written', 'pending'] as const, text: '流水、占券、超时事件都写好了。库存锁还没拿。' },
    { outcome: 'open' as const, marks: ['written', 'written', 'written', 'written', 'fail'] as const, text: '条件 quantity >= 购买数量 不成立。这一步失败。' },
    { outcome: 'rollback' as const, marks: ['rolled', 'rolled', 'rolled', 'rolled', 'rolled'] as const, text: '整笔回滚。外面仍然没有这张订单，券仍是未使用。' },
  ],
  wrong: [
    { outcome: 'open' as const, marks: ['pending', 'pending'] as const, text: '支付请求发现 expires_at 已经过了。订单仍是待支付。' },
    { outcome: 'open' as const, marks: ['written', 'written'] as const, text: '事务里已经写下关单和库存回补，但还没提交。' },
    { outcome: 'rollback' as const, marks: ['rolled', 'rolled'] as const, text: '闭包直接返回已过期。数据库把关单和回补一起撤销，订单卡在待支付。' },
  ],
  right: [
    { outcome: 'open' as const, marks: ['pending', 'pending'] as const, text: '同样是迟到的支付。过期判断在支付事务里做。' },
    { outcome: 'commit' as const, marks: ['kept', 'kept'] as const, text: '关单和回补先提交。订单变成已过期，库存回到账上。' },
    { outcome: 'commit' as const, marks: ['kept', 'kept'] as const, text: '提交之后再告诉买家订单已过期。错误返回不再碰到那些写入。' },
  ],
}

const failLabels = {
  stock: ['订单', '扣减流水', '券改为已用', '超时事件', '扣库存'],
  wrong: ['关单', '回补库存'],
  right: ['关单', '回补库存'],
}

function rowClass(mark: string) {
  if (mark === 'written') return 'border-emerald-800 bg-emerald-950 text-emerald-100'
  if (mark === 'kept') return 'border-emerald-700 bg-emerald-900 text-emerald-50'
  if (mark === 'fail') return 'border-red-800 bg-red-950 text-red-100 motion-safe:animate-pulse'
  if (mark === 'rolled') return 'border-zinc-800 bg-zinc-950 text-zinc-600 line-through'
  return 'border-zinc-800 text-zinc-500'
}

function FailPlay() {
  const [script, setScript] = useState<keyof typeof failScripts>('stock')
  const frames = failScripts[script]
  const play = useStepper(frames.length)
  const frame = frames[play.index]
  const labels = failLabels[script]
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-2">
        {(
          [
            ['stock', '库存不足'],
            ['wrong', '事务里直接报过期'],
            ['right', '先提交再报过期'],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            aria-pressed={script === id}
            onClick={() => {
              setScript(id)
              play.reset()
            }}
            className={cn(
              'rounded-full border px-3 py-1 text-sm',
              script === id ? 'border-zinc-100 bg-zinc-100 text-zinc-900' : 'border-zinc-700 text-zinc-300',
            )}
          >
            {label}
          </button>
        ))}
      </div>
      <Controls
        index={play.index}
        length={frames.length}
        playing={play.playing}
        onPlay={play.play}
        onPause={() => play.setPlaying(false)}
        onReset={play.reset}
      />
      <div
        className={cn(
          'space-y-2 rounded-lg border px-3 py-3 transition-colors motion-reduce:transition-none',
          frame.outcome === 'rollback' ? 'border-red-900' : frame.outcome === 'commit' ? 'border-emerald-900' : 'border-zinc-800',
        )}
      >
        <p className="text-xs text-zinc-500">
          {frame.outcome === 'rollback' ? '事务回滚' : frame.outcome === 'commit' ? '事务已提交' : '事务进行中'}
        </p>
        {labels.map((label, row) => (
          <div
            key={label}
            className={cn(
              'rounded-md border px-3 py-2 text-sm transition-colors motion-reduce:transition-none',
              rowClass(frame.marks[row] ?? 'pending'),
            )}
          >
            {label}
          </div>
        ))}
      </div>
      <p className="text-sm leading-6 text-zinc-200" aria-live="polite">
        {frame.text}
      </p>
    </div>
  )
}

type LockRow = { name: string; cells: [Cell, Cell] }

const deadScripts = {
  cycle: {
    heads: ['券行', '库存行'] as [string, string],
    frames: [
      {
        cycle: false,
        rows: [
          { name: '下单', cells: ['idle', 'idle'] },
          { name: '另一条路径', cells: ['idle', 'idle'] },
        ] as LockRow[],
        text: '两把锁都空着。下单的顺序是先券、后库存。',
      },
      {
        cycle: false,
        rows: [
          { name: '下单', cells: ['hold', 'idle'] },
          { name: '另一条路径', cells: ['idle', 'hold'] },
        ] as LockRow[],
        text: '若另一条路径先拿库存，两笔事务各握一把锁。',
      },
      {
        cycle: true,
        rows: [
          { name: '下单', cells: ['hold', 'wait'] },
          { name: '另一条路径', cells: ['wait', 'hold'] },
        ] as LockRow[],
        text: '下单等库存，另一条等券。谁也不释放，环就形成了。代码把所有路径都固定成先券后库存，避免走到这一帧。',
      },
    ],
  },
  same: {
    heads: ['券行', '库存行'] as [string, string],
    frames: [
      {
        cycle: false,
        rows: [
          { name: '下单', cells: ['idle', 'idle'] },
          { name: '退款回补', cells: ['idle', 'idle'] },
        ] as LockRow[],
        text: '两条路径都先要券，再要库存。',
      },
      {
        cycle: false,
        rows: [
          { name: '下单', cells: ['hold', 'hold'] },
          { name: '退款回补', cells: ['wait', 'idle'] },
        ] as LockRow[],
        text: '下单握着两把锁。回补只在券上排队，还碰不到库存。',
      },
      {
        cycle: false,
        rows: [
          { name: '下单', cells: ['gone', 'gone'] },
          { name: '退款回补', cells: ['hold', 'hold'] },
        ] as LockRow[],
        text: '下单提交并释放。回补按同一顺序拿到券和库存，然后提交。没有环。',
      },
    ],
  },
  refund: {
    heads: ['订单行', '退款单'] as [string, string],
    frames: [
      {
        cycle: false,
        rows: [
          { name: '商家裁决', cells: ['hold', 'idle'] },
          { name: '超时 worker', cells: ['idle', 'hold'] },
        ] as LockRow[],
        text: '若 worker 先锁退款单，而商家已经锁着订单、正要改退款单，两边接下来都要对方那一把。',
      },
      {
        cycle: true,
        rows: [
          { name: '商家裁决', cells: ['hold', 'wait'] },
          { name: '超时 worker', cells: ['wait', 'hold'] },
        ] as LockRow[],
        text: '互相等待。这是注释里写明要避开的锁序。',
      },
      {
        cycle: false,
        rows: [
          { name: '商家裁决', cells: ['hold', 'hold'] },
          { name: '超时 worker', cells: ['gone', 'idle'] },
        ] as LockRow[],
        text: '实际 worker 只锁订单，并且 SKIP LOCKED。商家握着订单时，这一笔被跳过，下一轮再看。',
      },
    ],
  },
}

function DeadPlay() {
  const [script, setScript] = useState<keyof typeof deadScripts>('cycle')
  const selected = deadScripts[script]
  const play = useStepper(selected.frames.length)
  const frame = selected.frames[play.index]
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-2">
        {(
          [
            ['cycle', '顺序相反'],
            ['same', '同一顺序'],
            ['refund', '退款两把锁'],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            aria-pressed={script === id}
            onClick={() => {
              setScript(id)
              play.reset()
            }}
            className={cn(
              'rounded-full border px-3 py-1 text-sm',
              script === id ? 'border-zinc-100 bg-zinc-100 text-zinc-900' : 'border-zinc-700 text-zinc-300',
            )}
          >
            {label}
          </button>
        ))}
      </div>
      <Controls
        index={play.index}
        length={selected.frames.length}
        playing={play.playing}
        onPlay={play.play}
        onPause={() => play.setPlaying(false)}
        onReset={play.reset}
      />
      <div className={cn('overflow-x-auto rounded-lg border p-3', frame.cycle ? 'border-red-900' : 'border-zinc-800')}>
        <div className="grid grid-cols-[7rem_1fr_1fr] gap-2 text-sm">
          <span />
          <span className="text-xs text-zinc-500">{selected.heads[0]}</span>
          <span className="text-xs text-zinc-500">{selected.heads[1]}</span>
          {frame.rows.map((row) => (
            <div key={row.name} className="contents">
              <span className="self-center text-zinc-300">{row.name}</span>
              {row.cells.map((cell, cellIndex) => (
                <span
                  key={cellIndex}
                  className={cn(
                    'rounded-md border px-2 py-3 text-center transition-colors motion-reduce:transition-none',
                    cellClass(cell, frame.cycle),
                  )}
                >
                  {cellLabel(cell)}
                </span>
              ))}
            </div>
          ))}
        </div>
        {frame.cycle && (
          <p className="mt-3 text-sm text-red-200 motion-safe:animate-pulse">两边各握着对方要的锁，形成等待环。</p>
        )}
      </div>
      <p className="text-sm leading-6 text-zinc-200" aria-live="polite">
        {frame.text}
      </p>
    </div>
  )
}

export function RaceVisual() {
  const [mode, setMode] = useState<'branch' | 'fail' | 'dead'>('branch')
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-2" role="tablist" aria-label="演示种类">
        {(
          [
            ['branch', '分支'],
            ['fail', '失败'],
            ['dead', '死锁'],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            role="tab"
            aria-selected={mode === id}
            onClick={() => setMode(id)}
            className={cn(
              'rounded-full border px-3 py-1 text-sm',
              mode === id ? 'border-zinc-100 bg-zinc-100 text-zinc-900' : 'border-zinc-700 text-zinc-300',
            )}
          >
            {label}
          </button>
        ))}
      </div>
      {mode === 'branch' && <BranchPlay />}
      {mode === 'fail' && <FailPlay />}
      {mode === 'dead' && <DeadPlay />}
    </div>
  )
}
