import type { ReactNode } from 'react'
import {
  AssemblyVisual,
  AuthVisual,
  CommerceVisual,
  EventVisual,
  FeedVisual,
  OrderVisual,
  PagesVisual,
  RefundVisual,
  UploadVisual,
} from './visuals'
import { RaceVisual } from './race-visual'

export const chapterIds = [
  'assembly',
  'auth',
  'upload',
  'commerce',
  'order',
  'refund',
  'races',
  'feed',
  'events',
  'pages',
] as const

export type ChapterId = (typeof chapterIds)[number]

export type Chapter = {
  id: ChapterId
  nav: string
  title: string
  lead: string
  foundation: string
  apply: string
  scene: ReactNode
  code: { file: string; lines: string; body: string }
}

export const chapters: Chapter[] = [
  {
    id: 'assembly',
    nav: '装配',
    title: '一个进程里的模块',
    lead: '先建立地图：请求从浏览器进来，经过哪几层，和哪三个外部系统说话。',
    foundation:
      '分层是为了让变化停在该停的地方。HTTP 的字段名、业务规则、SQL 条件各改各的。模块化单体把多个业务放在一个进程里，跨模块调用用本地接口，需要原子性的写入仍在一个数据库事务里。',
    apply:
      '接口写在调用方。商品模块要核销封面票，自己声明这个能力，main.go 把视频服务塞进去。Feed 要商品卡时用自己的 SQL，不引用商品包的 model。后台任务和 API 一起启动，停机时先停 HTTP，再按启动的反序停 worker。',
    scene: <AssemblyVisual />,
    code: {
      file: 'cmd/api/main.go',
      lines: '271–276',
      body: `productService := product.NewService(
    productRepository,
    storageClient,
    videoService, // ImageTicketValidator：封面防伪造复用视频模块的凭证核销
)`,
    },
  },
  {
    id: 'auth',
    nav: '认证',
    title: '谁在发这个请求',
    lead: '身份来自验签之后的上下文。资源是不是你的，在服务端用 owner ID 再对一次。',
    foundation:
      '访问令牌证明「这一小段时间里这是谁」，刷新令牌用来换新的一对。短令牌减少被盗后的窗口；刷新必须原子抢占，否则两个并发请求会发出两套都能用的新票。可选登录要区分「没带票」和「带了坏票」。',
    apply:
      'access 大约 15 分钟，refresh 大约 7 天，算法是 HS256。JWT_SECRET 和游标密钥在所有环境必填，生产里两者还得不同。作者、商家、买家是同一张 users 表。对不上归属时，和不存在一样返回 404。',
    scene: <AuthVisual />,
    code: {
      file: 'internal/middleware/auth.go',
      lines: '28–37',
      body: `// AuthOptional 可选认证：无 Authorization header → 匿名继续；
// 有 header 但无效（格式/签名/过期）→ 401。
func AuthOptional(manager *token.Manager) app.HandlerFunc {
    return func(c context.Context, ctx *app.RequestContext) {
        if string(ctx.GetHeader("Authorization")) == "" {
            ctx.Set(CtxUserIDKey, int64(0))
            ctx.Next(c)
            return
        }`,
    },
  },
  {
    id: 'upload',
    nav: '上传',
    title: '票证、封存、发布',
    lead: '大文件直传。数据库负责「这张票只能核销一次」，对象存储负责字节。两边没有分布式事务。',
    foundation:
      '预签名把上传权限收成一次有限的 PUT：限定键、大小、类型和过期时间。封存时再看对象的真实大小，并用 ETag 保证复制的是刚刚检查过的那一版。状态迁移的预检负责给出明确错误，真正互斥的是带原状态的条件更新。',
    apply:
      '创建成功直接是 PENDING_REVIEW。只有 PASSED 或 SKIPPED 能发布，SKIPPED 表示这次没验。删除和 object_delete_jobs 同一个事务。存在性防护只剩 Redis 负缓存；负缓存只记录数据库确认缺失的 ID，读失败放行到数据库。',
    scene: <UploadVisual />,
    code: {
      file: 'internal/video/repository.go',
      lines: '69–83',
      body: `query := r.db.WithContext(ctx).Model(&Video{}).
    Where("id = ? AND author_id = ? AND status = ?", id, authorID, string(from))
if to == StatusPublished {
    query = query.Where("media_probe_status IN ?", []string{
        string(ProbePassed), string(ProbeSkipped),
    })
}
res := query.Updates(...)
if res.RowsAffected == 0 {
    return ErrStatusConflict
}`,
    },
  },
  {
    id: 'commerce',
    nav: '商品',
    title: '价格、库存、券',
    lead: '三件事各有一个判定点。订单只保存判定当时的快照。',
    foundation:
      '价格以服务端目录为准，否则客户端可以改请求里的金额。库存用条件更新加流水：余额不能为负，同一业务键只生效一次。优惠集中在一个函数里，门槛、有效期、面额的规则才不会在下单、展示、对账三处各写一份。',
    apply:
      '一条已发布视频最多挂 5 个自己的在售商品。下架商品详情还在，SKU 列表为空。商家补货走库存模块的 Adjust；下单扣减写在订单自己的事务末尾。券只能用于同商家的单，取消、支付超时和退款成功会退券。',
    scene: <CommerceVisual />,
    code: {
      file: 'internal/coupon/service.go',
      lines: '144–158',
      body: `func DiscountFor(uc UserCoupon, c Coupon, orderAmount int64, now time.Time) (int64, error) {
    if uc.Status != UserCouponUnused {
        return 0, ErrUserCouponUsed
    }
    if now.Before(c.ValidFrom) || !now.Before(c.ValidTo) {
        return 0, ErrCouponExpired
    }
    if orderAmount < c.ThresholdAmount {
        return 0, ErrThresholdNotMet
    }
    discount := c.DiscountAmount
    if discount > orderAmount {
        discount = orderAmount
    }
    return discount, nil
}`,
    },
  },
  {
    id: 'order',
    nav: '下单',
    title: '一笔订单的事务',
    lead: '幂等键回答「是不是同一次尝试」。指纹回答「这一次尝试的内容有没有变」。',
    foundation:
      '重试在网络超时后必然发生。同键同内容应返回原结果；同键换内容必须冲突，否则调用方会以为换过的 SKU 已经生效。热点行放在事务尾部，是为了缩短持有这把锁的时间。锁顺序固定，避免一个事务先锁库存、另一个先锁券。',
    apply:
      '主路径是待支付、已支付、待收货、已完成。支付写下 OrderPaid，不动库存。发货写下自动确认时间。来源视频由客户端提交，服务端核对这件商品确实挂在这条已发布视频上。',
    scene: <OrderVisual />,
    code: {
      file: 'internal/order/service.go',
      lines: '169–175',
      body: `// fulfillmentTransitions 是「商家发货 / 买家确认收货」允许的迁移。
var fulfillmentTransitions = map[Status]map[Status]struct{}{
    StatusPaid:       {StatusFulfilling: {}},
    StatusFulfilling: {StatusCompleted: {}},
}`,
    },
  },
  {
    id: 'refund',
    nav: '退款',
    title: '三条截止时间',
    lead: '时间推动的状态变更要有事实来源。延迟消息可以提前跑，扫描和请求路径负责兜住。',
    foundation:
      '截止时间如果会被改写，就不宜只发一条到点执行的消息，因为发出去的消息收不回来。轮询用 SKIP LOCKED 让多个 worker 各领各的行。拒绝恢复哪一个状态，必须记在申请当时，而不是拒绝时再猜。',
    apply:
      '全额退款只接受已支付和待收货。完成后不再申请。已发货的同意退款不把库存加回去，因为货物没有回到仓库。退款处理期间不会被自动确认，扫描条件是状态仍为待收货。',
    scene: <RefundVisual />,
    code: {
      file: 'internal/order/refund_repository.go',
      lines: '384–398',
      body: `restored := *order.RefundFromStatus
changes := map[string]any{
    "status": string(restored), "refund_from_status": nil, "updated_at": at,
}
if restored == StatusFulfilling {
    deadline := *order.AutoCompleteAt
    if minimum := minAutoCompleteAt.UTC(); deadline.Before(minimum) {
        deadline = minimum
    }
    changes["auto_complete_at"] = deadline
}`,
    },
  },
  {
    id: 'races',
    nav: '并发',
    title: '分支、失败和死锁',
    lead: '按播放看三件事：同一行上两条命令怎么分出胜负，失败时哪些写入还在，以及两把锁顺序相反时为什么谁也走不了。',
    foundation:
      '分支是多条命令都想改同一份事实，提交之后只留下一种终态。失败要把这次尝试的副作用收干净；有的错误必须等事实提交之后再返回，否则调用方看到失败，库里的补救也没了。死锁是两边各握着对方下一步需要的锁，并且都不释放。',
    apply:
      '发货和申请退款都锁订单行，后拿到锁的人按已经提交的状态决定能不能继续。下单固定先锁券再扣库存。超时同意只锁订单，商家正握着这行时用 SKIP LOCKED 跳过。库存不足会回滚整笔订单。支付时发现过期，先提交关单，再返回已过期。',
    scene: <RaceVisual />,
    code: {
      file: 'internal/order/refund_repository.go',
      lines: '158–160',
      body: `// 只锁 orders（FOR UPDATE OF o SKIP LOCKED），不在这条 SELECT 里锁 refunds。
// DecideRefund 也是先锁订单再改退款单；如果这里先锁退款单，worker 和商家会互相等待。
// 商家正握着订单锁时，这一笔被跳过，等下一轮再看。`,
    },
  },
  {
    id: 'feed',
    nav: 'Feed',
    title: '游标和缓存',
    lead: '翻页要稳定，缓存要允许短暂过期，个性化字段要在共享结果之外填。',
    foundation:
      '偏移量翻页在有新内容插入时会跳或重复。用上一页最后一条的排序键继续取，顺序就固定了。签名防止调用方改这个位置。缓存的是大家共用的那一份；点赞数这类因人而异或变化很快的字段，在返回前单独合并。',
    apply:
      '关注流生产只用时间排序，因为实验中的热度公式依赖请求时刻，接不上这条游标。热榜是另一套分钟桶和冻结窗口。点赞用唯一键加 ON CONFLICT，只有真正插入或删除才改热度。',
    scene: <FeedVisual />,
    code: {
      file: 'internal/feed/tiered_cache.go',
      lines: '272–281',
      body: `func (c *TieredFeedCache) InvalidateFeedCache(ctx context.Context) error {
    if err := c.Del(ctx); err != nil {
        return err
    }
    if c.version != nil {
        return c.version.Bump(ctx)
    }
    return nil
}`,
    },
  },
  {
    id: 'events',
    nav: '事件',
    title: '事实、投递、对账',
    lead: '业务提交成功之后，统计可以晚一点，但不能在没有回执时假装已经记上。',
    foundation:
      '本地消息表把「事实」和「待发送事件」放进同一个事务。投递至少一次，所以消费端用事件 ID 做幂等。回执和投影同事务：没有回执就等于投影没生效。对账比较两边的结果，发现差异，不直接改账。',
    apply:
      '支付按支付日累加毛额，退款成功按退款日单列，申请退款不减毛额。作者打开个人页时读的是订单表实时聚合，日销表目前只给对账看。九项检查覆盖库存流水、退款单、支付、券、金额拆分和投递回执。',
    scene: <EventVisual />,
    code: {
      file: 'internal/outbox/repository.go',
      lines: '185–191',
      body: `// Enqueue 在调用方事务内写入事件。
// 传入的 *gorm.DB 必须是业务事务的句柄。
func Enqueue(tx *gorm.DB, event Event) error {
    if tx == nil {
        return errors.New("outbox enqueue requires a transaction handle")
    }
    return tx.Create(&event).Error
}`,
    },
  },
  {
    id: 'pages',
    nav: '页面',
    title: '页面接到哪条规则',
    lead: '前端用同一套技术：文件路由、生成的 API 类型、Query 缓存。每个页面只接它真正调用的那一段后端。',
    foundation:
      '单页应用把登录态放在客户端，请求时带上访问令牌。令牌过期就合并成一次刷新再重试。乐观更新先改缓存里的计数，失败再滚回去。生成类型让路径和字段与 OpenAPI 一起变。',
    apply:
      '令牌在 localStorage，多标签用 BroadcastChannel 同步。退出只清空点退出的那个标签的查询缓存。优惠券、热榜、商品搜索和上下架还没有页面。点赞在 Feed 上用本标签内存补「我是否赞过」，详情页才有服务端字段。',
    scene: <PagesVisual />,
    code: {
      file: 'web/src/routes/learn.tsx',
      lines: '本页',
      body: `export const Route = createFileRoute('/learn')({
  validateSearch: learnSearchSchema,
  component: LearnRoute,
})`,
    },
  },
]
