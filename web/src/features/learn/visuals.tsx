import { useState } from 'react'
import { cn } from '../../lib/cn'

function Chip({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: string
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        'rounded-full border px-3 py-1 text-left text-sm transition-colors',
        active
          ? 'border-zinc-100 bg-zinc-100 text-zinc-900'
          : 'border-zinc-700 text-zinc-300 hover:bg-zinc-800',
      )}
    >
      {children}
    </button>
  )
}

function Panel({ label, children }: { label: string; children: string }) {
  return (
    <div className="mt-4 rounded-lg bg-zinc-950/70 px-3 py-3">
      <p className="text-xs text-zinc-500">{label}</p>
      <p className="mt-1 text-sm leading-6 text-zinc-200">{children}</p>
    </div>
  )
}

export function AssemblyVisual() {
  const nodes = {
    browser: '浏览器只拿预签名地址，视频字节直接交给对象存储。',
    web: 'React 页面负责路由、登录态和乐观更新。类型从 OpenAPI 生成，页面不手写第二套响应形状。',
    api: '一个 Hertz 进程同时接 HTTP 和后台任务。模块之间的实现只在 cmd/api/main.go 里接上。',
    pg: '订单、库存、券、Outbox、回执在同一个库。需要一起成功的写入用本地事务。',
    redis: '缓存、限流、刷新票据抢占，以及事件流。它挂了，限流会放行，主路径还在。',
    rustfs: '私有桶。上传写入 staging/，封存后再复制到正式键。播放地址是短时预签名。',
  } as const
  const [node, setNode] = useState<keyof typeof nodes>('api')
  const layers = {
    handler: '读路径和 JSON，从中间件拿 user_id，把已知领域错误映射成 4xx。对不上的错误是 500。',
    service: '判断规则和状态能不能迁。预检先读，是为了区分 404 和冲突；并发安全靠后面的条件更新。',
    repo: 'SQL 与事务。ORM 错误翻译成领域哨兵。表上的 model 不出这个包。',
  } as const
  const [layer, setLayer] = useState<keyof typeof layers>('service')

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        {(
          [
            ['browser', '浏览器'],
            ['web', 'React'],
            ['api', 'Hertz 进程'],
          ] as const
        ).map(([id, label]) => (
          <Chip key={id} active={node === id} onClick={() => setNode(id)}>
            {label}
          </Chip>
        ))}
      </div>
      <div className="flex flex-wrap gap-2 pl-0 sm:pl-8">
        {(
          [
            ['pg', 'PostgreSQL'],
            ['redis', 'Redis'],
            ['rustfs', 'RustFS'],
          ] as const
        ).map(([id, label]) => (
          <Chip key={id} active={node === id} onClick={() => setNode(id)}>
            {label}
          </Chip>
        ))}
      </div>
      <Panel label="这个框负责什么">{nodes[node]}</Panel>
      <div className="flex flex-wrap gap-2">
        {(
          [
            ['handler', 'handler'],
            ['service', 'service'],
            ['repo', 'repository'],
          ] as const
        ).map(([id, label]) => (
          <Chip key={id} active={layer === id} onClick={() => setLayer(id)}>
            {label}
          </Chip>
        ))}
      </div>
      <Panel label="一层只做自己的事">{layers[layer]}</Panel>
    </div>
  )
}

export function AuthVisual() {
  const cases = {
    anon: '没有 Authorization。AuthOptional 把 user_id 写成 0，公开 Feed 继续。',
    bad: '带了头，但签名错、过期，或拿 refresh 当 access。请求在中间件停住，401。',
    refresh: '两个标签同时拿同一张 refresh 来换。Redis SET NX 只让一个 jti 抢占成功，另一个失败。抢到之后如果签发失败，旧票也回不去。',
    access: '每个请求只验 access 的签名和过期。账号刚被停用时，这张 access 在过期前仍然可用；下一次刷新才查账户是否还能用。',
  } as const
  const [id, setId] = useState<keyof typeof cases>('bad')
  return (
    <div>
      <div className="flex flex-wrap gap-2">
        <Chip active={id === 'anon'} onClick={() => setId('anon')}>
          不带票据
        </Chip>
        <Chip active={id === 'bad'} onClick={() => setId('bad')}>
          票据无效
        </Chip>
        <Chip active={id === 'refresh'} onClick={() => setId('refresh')}>
          并发刷新
        </Chip>
        <Chip active={id === 'access'} onClick={() => setId('access')}>
          只验 access
        </Chip>
      </div>
      <Panel label="这一下会发生什么">{cases[id]}</Panel>
    </div>
  )
}

const uploadSteps = [
  {
    title: '申请票',
    body: '服务端按类型限制大小，把 Content-Length 和 Content-Type 写进预签名。对象键给客户端时不带前缀，签名路径是 staging/ 加上这个键。票在签名成功后才入库。',
  },
  {
    title: '浏览器 PUT',
    body: '文件不经过 Go 进程。PUT 必须带上签名里的那组头。正式键此时还不存在。',
  },
  {
    title: '封存',
    body: '事务先锁票证行。StatObject 核对实际大小，再按当时的 ETag 条件复制到正式键。复制成功之后如果数据库回滚，正式键上会留下没有视频行的对象。',
  },
  {
    title: '校验',
    body: '新建视频是 PENDING_REVIEW，媒体状态先是 PENDING。探测通过写成 PASSED；工具缺失或探测关闭写成 SKIPPED。坏文件是 REJECTED。',
  },
  {
    title: '发布',
    body: '更新语句要求作者匹配、原状态是 PENDING_REVIEW，并且媒体是 PASSED 或 SKIPPED。影响 0 行就是冲突。数据库 CHECK 再挡一层。',
  },
]

export function UploadVisual() {
  const [step, setStep] = useState(0)
  const current = uploadSteps[step]
  return (
    <div>
      <ol className="flex flex-col gap-2 sm:flex-row sm:flex-wrap">
        {uploadSteps.map((item, index) => (
          <li key={item.title}>
            <button
              type="button"
              onClick={() => setStep(index)}
              className={cn(
                'flex w-full items-center gap-2 rounded-lg border px-3 py-2 text-left text-sm sm:w-auto',
                index === step
                  ? 'border-zinc-100 bg-zinc-100 text-zinc-900'
                  : 'border-zinc-800 text-zinc-300 hover:bg-zinc-800',
              )}
            >
              <span className="font-mono text-xs opacity-70">{index + 1}</span>
              {item.title}
            </button>
          </li>
        ))}
      </ol>
      <Panel label={current.title}>{current.body}</Panel>
    </div>
  )
}

export function CommerceVisual() {
  const [stock, setStock] = useState(5)
  const [seenKeys, setSeenKeys] = useState<string[]>([])
  const [note, setNote] = useState('余额 5。补货和扣减都先看条件，再改数字。')

  function restock() {
    const key = 'ADJUST:sku:1'
    if (seenKeys.includes(key)) {
      setNote('同一把业务键已经有流水。余额不动，这次是重放。')
      return
    }
    setSeenKeys((keys) => [...keys, key])
    setStock((n) => n + 2)
    setNote('流水插入成功，再执行 quantity + 2 >= 0。余额变成 7。')
  }

  function order(qty: number) {
    setStock((n) => {
      if (n < qty) {
        setNote(`条件 quantity >= ${qty} 不成立。订单、流水、券占用都留在事务里一起回滚，余额仍是 ${n}。`)
        return n
      }
      setNote(`下单事务的最后一步扣掉 ${qty}。前面的订单行和超时事件已经写好，这一步失败的话它们也会撤销。`)
      return n - qty
    })
  }

  const quotes = {
    low: '原价 80 分，门槛 100 分。DiscountFor 直接拒绝，订单不会插入。',
    ok: '原价 120 分，满 100 减 30。优惠 30，实付 90。实付必须大于 0。',
    foreign: '券的 merchant_id 必须等于 SKU 的商家。别的店的券在锁持券行时就结束。',
  } as const
  const [quote, setQuote] = useState<keyof typeof quotes>('ok')

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <p className="font-mono text-3xl text-zinc-100">{stock}</p>
        <p className="text-xs text-zinc-500">当前库存件数</p>
      </div>
      <div className="flex flex-wrap gap-2">
        <Chip active={false} onClick={restock}>
          商家补货 +2
        </Chip>
        <Chip active={false} onClick={() => order(2)}>
          下单扣 2
        </Chip>
        <Chip active={false} onClick={() => order(100)}>
          下单扣 100
        </Chip>
        <Chip
          active={false}
          onClick={() => {
            setStock(5)
            setSeenKeys([])
            setNote('余额回到 5，业务键清空。')
          }}
        >
          重置
        </Chip>
      </div>
      <Panel label="库存这一步">{note}</Panel>
      <div className="flex flex-wrap gap-2">
        <Chip active={quote === 'low'} onClick={() => setQuote('low')}>
          没到门槛
        </Chip>
        <Chip active={quote === 'ok'} onClick={() => setQuote('ok')}>
          满减成立
        </Chip>
        <Chip active={quote === 'foreign'} onClick={() => setQuote('foreign')}>
          别的商家的券
        </Chip>
      </div>
      <Panel label="折扣只从 DiscountFor 出来">{quotes[quote]}</Panel>
    </div>
  )
}

const orderSteps = [
  '对用户和幂等键加事务级咨询锁，再查有没有原订单。',
  '指纹相同就带回原单。指纹含 SKU、数量、来源和券，不含幂等键本身。换了内容就是冲突。',
  '读取当前可售 SKU。单价用这一行，并抄进订单明细。客户端传来的价格不参与。',
  '要用券就先锁持券行。有效期用现在，不用订单的支付截止时间。',
  '插入订单、明细和扣减流水，把券改成 USED，并写入 available_at 等于支付截止时间的超时事件。',
  '最后按 quantity >= 购买数量 更新库存。成功就提交；失败则上面几步全部回滚。',
]

export function OrderVisual() {
  const [step, setStep] = useState(0)
  const [stockOk, setStockOk] = useState(true)
  const graph = {
    full: '完整图描述状态之间能不能到达，包含退款边。拒绝退款会从 REFUND_REQUESTED 回到申请前的 PAID 或 FULFILLING。',
    fulfill: '发货和确认只用窄表：PAID → FULFILLING，FULFILLING → COMPLETED。退款处理中的订单在这张表上没有出口。',
  } as const
  const [graphId, setGraphId] = useState<keyof typeof graph>('fulfill')
  const atEnd = step === orderSteps.length - 1
  const outcome = !atEnd
    ? '还在同一个事务里，外面还看不到这张订单。'
    : stockOk
      ? '条件更新影响了一行，事务提交。待支付订单、占券和超时事件一起可见。'
      : '库存条件没满足。事务回滚，不留下半张订单，券也仍是 UNUSED。'

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-2">
        {orderSteps.map((_, index) => (
          <Chip key={index} active={step === index} onClick={() => setStep(index)}>
            {String(index + 1)}
          </Chip>
        ))}
      </div>
      <p className="text-sm leading-6 text-zinc-200">{orderSteps[step]}</p>
      {atEnd && (
        <div className="flex flex-wrap gap-2">
          <Chip active={stockOk} onClick={() => setStockOk(true)}>
            库存够
          </Chip>
          <Chip active={!stockOk} onClick={() => setStockOk(false)}>
            库存不够
          </Chip>
        </div>
      )}
      <Panel label="事务结果">{outcome}</Panel>
      <div className="flex flex-wrap gap-2">
        <Chip active={graphId === 'full'} onClick={() => setGraphId('full')}>
          完整状态图
        </Chip>
        <Chip active={graphId === 'fulfill'} onClick={() => setGraphId('fulfill')}>
          履约命令
        </Chip>
      </div>
      <Panel label="两张表各管一件事">{graph[graphId]}</Panel>
    </div>
  )
}

export function RefundVisual() {
  const clocks = {
    pay: '支付截止默认 30 分钟，字段是 expires_at。到点关单、回补库存、退券。事件、轮询、迟到的支付请求都调用同一个关单函数。迟到的支付必须先提交关单，再返回已过期。',
    receive: '发货时写下 auto_complete_at，默认 7 天后。worker 用 SKIP LOCKED 把仍是 FULFILLING 且已到期的订单收成完成。这一步不退券。',
    respond: '申请退款时写下 respond_by，默认 48 小时。到期后锁订单，再走和商家同意同一条路径。来源是 TIMEOUT，decided_by 为空，退券。',
  } as const
  const [clock, setClock] = useState<keyof typeof clocks>('receive')
  const restores = {
    paid: '申请前是 PAID。拒绝后回到 PAID，可以继续发货。当时还没有自动确认时间。同意退款会回补库存。',
    fulfilling:
      '申请前是 FULFILLING。拒绝后回到 FULFILLING。自动确认时间只顺延：剩余不足 3 天就补到拒绝后 3 天，更长就保持原截止。同意退款不回补库存。',
  } as const
  const [from, setFrom] = useState<keyof typeof restores>('fulfilling')

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-2">
        <Chip active={clock === 'pay'} onClick={() => setClock('pay')}>
          支付 30 分钟
        </Chip>
        <Chip active={clock === 'receive'} onClick={() => setClock('receive')}>
          收货 7 天
        </Chip>
        <Chip active={clock === 'respond'} onClick={() => setClock('respond')}>
          退款应答 48 小时
        </Chip>
      </div>
      <Panel label="这条截止时间">{clocks[clock]}</Panel>
      <div className="flex flex-wrap gap-2">
        <Chip active={from === 'paid'} onClick={() => setFrom('paid')}>
          未发货就申请
        </Chip>
        <Chip active={from === 'fulfilling'} onClick={() => setFrom('fulfilling')}>
          已发货再申请
        </Chip>
      </div>
      <Panel label="拒绝之后回到申请前">{restores[from]}</Panel>
    </div>
  )
}

const pageOne = [
  { id: 'a', title: '第一条已发布视频' },
  { id: 'b', title: '第二条' },
  { id: 'c', title: '页尾，游标指在这里' },
]
const pageTwo = [
  { id: 'd', title: '更早的一条' },
  { id: 'e', title: '再早的一条' },
]

export function FeedVisual() {
  const [page, setPage] = useState<1 | 2>(1)
  const items = page === 1 ? pageOne : pageTwo
  const cache = {
    hit: '只有不带游标、页大小为默认 20 的第一页进缓存。点赞数和商品卡在命中之后现查，先复制共享结果再填。',
    invalidate: '发布时先删本机 L1，再删 Redis，删除成功后才把 feed:version 加一。版本读失败时别的进程继续用旧首页。',
    follow: '关注流首页按用户存。发布不去遍历粉丝的键，那些页靠 TTL，以及关注、取关时删掉操作者自己的首页。',
    hot: '热榜是冻结窗口，游标里有窗口终点、分数和视频 ID。首页降级成时间流之后，后面的游标继续是 time-v1。',
  } as const
  const [cacheId, setCacheId] = useState<keyof typeof cache>('hit')

  return (
    <div className="space-y-4">
      <ol className="divide-y divide-zinc-800 rounded-lg border border-zinc-800">
        {items.map((item, index) => {
          const tail = index === items.length - 1
          return (
            <li key={item.id} className="flex items-center justify-between px-3 py-2 text-sm">
              <span>{item.title}</span>
              {tail && page === 1 && (
                <span className="font-mono text-xs text-amber-200">cursor</span>
              )}
            </li>
          )
        })}
      </ol>
      <div className="flex flex-wrap gap-2">
        <Chip active={page === 2} onClick={() => setPage(2)}>
          用页尾游标要下一页
        </Chip>
        <Chip active={page === 1} onClick={() => setPage(1)}>
          回到第一页
        </Chip>
      </div>
      <Panel label="游标的意思">
        {page === 1
          ? '签名游标表示上一页最后一条的 (published_at, id)，版本是 time-v1。它是位置，不是一张会过期的内容快照。'
          : '下一页从这条位置往更早取。签名不对，或版本不是当前排序，请求被拒绝。'}
      </Panel>
      <div className="flex flex-wrap gap-2">
        <Chip active={cacheId === 'hit'} onClick={() => setCacheId('hit')}>
          首页缓存
        </Chip>
        <Chip active={cacheId === 'invalidate'} onClick={() => setCacheId('invalidate')}>
          发布后失效
        </Chip>
        <Chip active={cacheId === 'follow'} onClick={() => setCacheId('follow')}>
          关注流
        </Chip>
        <Chip active={cacheId === 'hot'} onClick={() => setCacheId('hot')}>
          热榜窗口
        </Chip>
      </div>
      <Panel label="缓存里放什么">{cache[cacheId]}</Panel>
    </div>
  )
}

export function EventVisual() {
  const cases = {
    enqueue: 'Enqueue 只接受事务句柄。订单提交时，支付事实和 OrderPaid 一起出现；回滚时事件也不在。',
    republish: '中继先发到 Redis Stream，再把库状态改成已发布。发布成功、标记失败时，租约到期后同一 event_id 会再发一次。',
    dead: '中继重试耗尽，这一行变成 PostgreSQL 里的 DEAD。用 Outbox 命令重新排队。它和消费失败不是同一个地方。',
    ack: '消费者先插入回执。新回执才执行投影，两者同一个事务。提交之后再 XACK。重复投递看到回执就跳过业务写入。',
    dlq: '处理失败则不 ACK，消息留在待确认列表。认领次数超限后进入 Redis 上的 DLQ，再从原流确认掉。',
  } as const
  const [id, setId] = useState<keyof typeof cases>('ack')
  return (
    <div>
      <div className="flex flex-wrap gap-2">
        <Chip active={id === 'enqueue'} onClick={() => setId('enqueue')}>
          同事务写入
        </Chip>
        <Chip active={id === 'republish'} onClick={() => setId('republish')}>
          至少一次
        </Chip>
        <Chip active={id === 'dead'} onClick={() => setId('dead')}>
          库内 DEAD
        </Chip>
        <Chip active={id === 'ack'} onClick={() => setId('ack')}>
          回执后确认
        </Chip>
        <Chip active={id === 'dlq'} onClick={() => setId('dlq')}>
          消费侧 DLQ
        </Chip>
      </div>
      <Panel label="消息现在在哪">{cases[id]}</Panel>
    </div>
  )
}

const pages = {
  feed: '首页刷推荐或关注。推荐游标进 URL。播放上报在卡片可见时发出。热榜接口后端有，这个页面没调用。',
  upload: '上传页申请票、直传、再 POST /videos。这一步就是封存并落入待发布。校验状态在个人页和详情里等。',
  product: '商品页选一个 SKU，下单幂等键是随机 UUID，内容变了才换。支付单号记在这次页面状态里。',
  orders: '订单列表补支付用 sim-订单号，同一订单重试是同一套支付身份。发货、确认、退款申请都在这里。',
  me: '个人页看实时销售聚合，毛额含后来退掉的订单。日销投影表没有读接口，页面不读它。',
} as const

export function PagesVisual() {
  const [id, setId] = useState<keyof typeof pages>('feed')
  return (
    <div>
      <div className="flex flex-wrap gap-2">
        <Chip active={id === 'feed'} onClick={() => setId('feed')}>
          /
        </Chip>
        <Chip active={id === 'upload'} onClick={() => setId('upload')}>
          /upload
        </Chip>
        <Chip active={id === 'product'} onClick={() => setId('product')}>
          /product/$id
        </Chip>
        <Chip active={id === 'orders'} onClick={() => setId('orders')}>
          /orders
        </Chip>
        <Chip active={id === 'me'} onClick={() => setId('me')}>
          /me
        </Chip>
      </div>
      <Panel label="这个页面接到哪条规则">{pages[id]}</Panel>
    </div>
  )
}
