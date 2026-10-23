import type { Scenario } from './scenario'

export const feedScenarios: Scenario[] = [
  {
    id: 'cursor',
    title: '游标是上一页的末尾位置',
    foundation: '用偏移量翻页时，新内容插入会跳过或重复。用上一页最后一条的排序键继续取，顺序就固定了。签名防止调用方改这个位置。',
    apply: '全局 Feed 按发布时间加 ID 倒序，版本是 time-v1。签名坏、版本不对就拒绝。只有不带游标且页大小为默认 20 的第一页进缓存。',
    code: {
      file: 'internal/feed/cursor.go',
      lines: '48–57',
      body: `// Encode：JSON → Base64URL → HMAC-SHA256。
// 格式 payload.signature。游标表示上一页末项。`,
    },
  },
  {
    id: 'dynamic',
    title: '点赞数不进共享缓存',
    foundation: '大家共用的缓存不能被某一次请求改掉。变化快或因人而异的字段，在复制出私有结果之后再填。',
    apply: '缓存里是视频主体和预签名地址。命中后先 cloneFeedResult，再按本页 ID 合并点赞数和商品卡。直接改共享结果会把一个用户的数字写进别人的响应。',
    code: {
      file: 'internal/feed/service.go',
      lines: '418–429',
      body: `func cloneFeedResult(result *FeedResult) *FeedResult {
    cloned := *result
    cloned.Items = make([]FeedItem, len(result.Items))
    copy(cloned.Items, result.Items)
    return &cloned
}`,
    },
  },
  {
    id: 'invalidate',
    title: '失效顺序是本地、远端、版本号',
    foundation: '先加版本、后删缓存，别的进程可能把旧值按新版本写回去。删远端失败时也不该假装大家都会刷新。',
    apply: '先删本机 L1，再删 Redis，删除成功后才把 feed:version 加一。版本读失败时继续用旧值。所以探测间隔不是所有故障下的陈旧上限。L1 的 TTL 不超过 L2。',
    code: {
      file: 'internal/feed/tiered_cache.go',
      lines: '274–280',
      body: `func (c *TieredFeedCache) InvalidateFeedCache(ctx context.Context) error {
    if err := c.Del(ctx); err != nil { return err }
    if c.version != nil { return c.version.Bump(ctx) }
    return nil
}`,
    },
  },
  {
    id: 'following',
    title: '发布不去遍历每个粉丝的缓存',
    foundation: '一条内容的读者可能远多于作者愿意同步删除的键。按用户存的首页靠过期，以及这个用户自己的关注变化来删。',
    apply: '关注流是完整时间线，生产只用时间排序。不超过 32 个作者时按作者取一截再归并，更多时走全局时间索引。关注或取关只删操作者自己的首页。',
    code: {
      file: 'internal/feed/cache.go',
      lines: '19',
      body: `followingCachePrefix = "feed:following:full-v1"`,
    },
  },
  {
    id: 'hot',
    title: '热榜是冻住的窗口，不是即时算分',
    foundation: '分数依赖「现在」时，翻页的下一页和上一页不是同一个全序。要翻页，就得把一次排名冻住，游标指在那次排名里。',
    apply: '点赞和播放把分数加进分钟桶。每分钟合成最近 60 分钟，最多 2000 条，窗口活 10 分钟，已有窗口不重写。首页没有窗口时改走时间流，之后的游标继续是 time-v1。实验用的 HotRanker 声明自己不能翻页，配置和关注流都会拒绝它。',
    code: {
      file: 'internal/feed/hot_service.go',
      lines: '45–48',
      body: `// 会话一旦降级成时间流，Redis 恢复后也继续用这条时间游标。
if parsed.RankVersion == CurrentRankVersion {
    return s.List(ctx, limit, cursorRaw)
}`,
    },
  },
  {
    id: 'like',
    title: '只有真的插入或删除才改热度',
    foundation: '唯一关系加上冲突时什么也不做，可以让重复点击安全。热度如果在重放时也加，计数会被重试放大。',
    apply: '点赞主键是用户加视频，插入还要求视频仍发布。RowsAffected 大于 0 才给热榜加分。关注同理，不能关注自己，成功后只删这个关注者的关注流首页。',
    code: {
      file: 'internal/interaction/repository.go',
      lines: '46–53',
      body: `INSERT INTO likes (user_id, video_id)
SELECT ?, ?
WHERE EXISTS (SELECT 1 FROM videos WHERE id = ? AND status = 'PUBLISHED')
ON CONFLICT (user_id, video_id) DO NOTHING`,
    },
  },
  {
    id: 'play',
    title: '同一次播放的键必须是同一笔事实',
    foundation: '客户端重试会用同一个事件号。号相同但视频或用户不同，就是两笔事实抢一个键，必须冲突，不能当成成功。',
    apply: '只有已发布视频接受上报。冲突后比较用户、视频、会话，以及客户端是否提供了播放时间。trace 和入库时间会变，不参与比较。一致则不再加播放热度。',
    code: {
      file: 'internal/video/service.go',
      lines: '646–648',
      body: `if a.UserID != b.UserID || a.VideoID != b.VideoID ||
    a.PlayedAtProvided != b.PlayedAtProvided {
    return false
}`,
    },
  },
]

export const eventScenarios: Scenario[] = [
  {
    id: 'enqueue',
    title: '事实和事件一起提交',
    foundation: '业务成功之后再单独发消息，进程在中间崩溃就会丢事件。事件写在同一个事务里，提交和回滚就和事实一致。',
    apply: 'Enqueue 拒绝空事务。下单写 OrderTimeout，支付写 OrderPaid，退款成功写 OrderRefunded。投递是至少一次，不宣称恰好一次。',
    code: {
      file: 'internal/outbox/repository.go',
      lines: '185–191',
      body: `func Enqueue(tx *gorm.DB, event Event) error {
    if tx == nil {
        return errors.New("outbox enqueue requires a transaction handle")
    }
    return tx.Create(&event).Error
}`,
    },
  },
  {
    id: 'relay',
    title: '先发出去，再标记已发布',
    foundation: '标记成功但发送失败会丢消息；发送成功但标记失败会再发一次。至少一次选择后者，由消费端按事件号认重。',
    apply: '中继领取已到期的 PENDING，跳过还没到支付截止时间的超时事件。积压看已到期数量。重试耗尽后这一行变成库内 DEAD，用 Outbox 命令重新排队。',
    code: {
      file: 'internal/outbox/relay.go',
      lines: '12–14',
      body: `// 投递语义是至少一次：先投递、后标记。
// 两者之间崩溃会重发同一 event_id。`,
    },
  },
  {
    id: 'dlq',
    title: '消费失败进另一条流',
    foundation: '发不出去和消费不了是两个地方。恢复命令如果混用，会把还能重投的行和已经确认掉的消息搞反。',
    apply: '处理失败不 ACK，留在待确认列表，空闲后重新认领。次数超限写入 Redis 上的 DLQ，再从原流确认。无法解码的消息直接进 DLQ。',
    code: {
      file: 'internal/outbox/consumer.go',
      lines: '276–280',
      body: `if txErr != nil {
    return // 不 ACK：留在 PEL，等认领重投或进 DLQ
}`,
    },
  },
  {
    id: 'receipt',
    title: '没有回执就等于投影没记上',
    foundation: '投影和「我已经处理过」必须同一个事务。否则投影写上了、回执没写，重投会再加一遍；或者回执写了、投影没写，再也补不回来。',
    apply: '先插入 consumed_events，主键是消费组加事件号。新回执才执行业务。提交之后再 XACK。删掉已有回执再重放，会让同一笔支付再累加。',
    code: {
      file: 'internal/outbox/consumer.go',
      lines: '261–274',
      body: `inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
if inserted.RowsAffected == 0 { duplicated = true; return nil }
return handler.Handle(ctx, tx, event)`,
    },
  },
  {
    id: 'gmv',
    title: '毛额含后来退掉的订单',
    foundation: '申请退款还没成功时，成交并没有取消。成功退款记在退款那天，不去改成交那天的毛额，净额由展示层相减。',
    apply: '投影按支付时间的 UTC 日累加订单数、件数和毛额，按退款成功日累加退款额。没有来源作者的订单跳过。个人页的销售接口读订单表，不读这张日销表。',
    code: {
      file: 'internal/attribution/projector.go',
      lines: '73–79',
      body: `DoUpdates: clause.Assignments(map[string]any{
    "order_count": gorm.Expr("author_sales_daily.order_count + ?", row.OrderCount),
    "gmv": gorm.Expr("author_sales_daily.gmv + ?", row.GMV),
})`,
    },
  },
  {
    id: 'checks',
    title: '九项对账只报告差异',
    foundation: '对账发现两边不一致时，自动改库存或改钱会把第二次错误写进去。它应该列出差异，把修复留给明确的维护动作。',
    apply: '九项是库存余额、订单流水、退款单、支付、日销、发券、用券、金额拆分、投递回执。改日销口径时，投影、这项检查和重建插入要一起改。投递项按事件找任意回执；重放和重建必须带消费组名。',
    code: {
      file: 'internal/reconcile/repository.go',
      lines: '73–84',
      body: `// checks：inventory_balance、order_stock_ledger、
// refund_order_consistency、order_payment_consistency、
// author_sales_projection、coupon_issuance、coupon_usage、
// order_amount_split、outbox_delivery`,
    },
  },
  {
    id: 'rebuild',
    title: '重建投影要回执齐全并且立刻拿到锁',
    foundation: '在还有未消费事件时清空日销再重算，旧事件随后再加一遍，毛额会翻倍。锁等不及就该放弃，而不是和线上写入抢。',
    apply: '必须指定真实消费组，该组对全部支付和退款事件都有回执，并且能立刻拿到相关表的 NOWAIT 锁。通过后删掉日销表，按和当前对账相同的口径重算。',
    code: {
      file: 'internal/reconcile/repository.go',
      lines: '349–360',
      body: `// 仅在目标组已消费全部支付/退款事件时重建。
LOCK TABLE orders, order_items, refunds, outbox_events,
    consumed_events, author_sales_daily
IN SHARE ROW EXCLUSIVE MODE NOWAIT`,
    },
  },
]

export const pageScenarios: Scenario[] = [
  {
    id: 'map',
    title: '每个页面只接它调用的那段后端',
    foundation: '页面是场景的入口，不是第二套业务规则。金额、状态能不能迁、库存够不够，仍由服务端判定。',
    apply: '首页是推荐和关注。上传页到创建视频为止。商品页下单并立刻模拟支付。订单页做支付、取消、发货、确认和退款。个人页看实时销售。类型从 OpenAPI 生成。',
    code: {
      file: 'web/src/routes',
      lines: '页面',
      body: `// /  /upload  /product/$id  /orders  /orders/$id  /me  /video/$id  /user/$id`,
    },
  },
  {
    id: 'token',
    title: '访问票据存在本地，刷新合并成一次',
    foundation: '浏览器里的令牌挡不住同源脚本。过期后如果每个请求各自去换刷新票据，会把一次性票据用乱。',
    apply: 'localStorage 键名 tokmall-auth。业务接口 401 时刷新逻辑 singleflight，换到新 access 再重试一次。大约 13 到 14 分钟会提前刷新。多标签用 BroadcastChannel 同步，不负责让刷新互相排队。',
    code: {
      file: 'web/src/lib/auth-refresh.ts',
      lines: '刷新',
      body: `// refreshAccessToken 合并并发刷新。
// 失败时若其他标签已经换过票据，用 store 里的最新值再试一次。`,
    },
  },
  {
    id: 'logout',
    title: '退出只清空这个标签的查询缓存',
    foundation: '登录态可以广播到其他标签，内存里的列表不会自动跟着消失。受保护页面还要在令牌消失时自己离开。',
    apply: '点退出的标签会 queryClient.clear()。另一个标签只是令牌被清掉。令牌消失时会离开个人页和上传页；停在订单页要等下一次 401。回跳只接受站内路径。',
    code: {
      file: 'web/src/components/layout/header.tsx',
      lines: '退出',
      body: `clear()
queryClient.clear()
router.navigate({ to: '/' })`,
    },
  },
  {
    id: 'pay-key',
    title: '两处支付用两种方式生成同一类身份',
    foundation: '幂等要求同一次尝试用同一把键。键怎么生成可以不同，但不能中途换成另一套事实。',
    apply: '商品页的下单键是随机 UUID，SKU、数量或来源变了才换。订单列表补支付用 sim-订单号，同一订单重试是同一套支付单号。发货和确认没有客户端幂等键。',
    code: {
      file: 'web/src/routes/orders.tsx',
      lines: '补支付',
      body: `// 商品页：randomUUID()，内容变了才换键
// 订单列表：payment 身份 sim-\${order_no}`,
    },
  },
  {
    id: 'liked',
    title: '信息流上没有「我是否赞过」',
    foundation: '公开列表如果带上每个观众的点赞状态，就不能被大家共用。本机可以暂时记住刚刚的操作，刷新之后以服务端详情为准。',
    apply: 'Feed 用本标签内存补已赞样子，整页刷新后会丢。详情页才有服务端 is_liked。点赞请求把目标状态放进 mutate 的参数。上传成功只是待发布，发布按钮看 can_publish。',
    code: {
      file: 'web/src/lib/liked-store.ts',
      lines: '本标签',
      body: `// 公开 Feed 不返回 is_liked。
// liked-store 只补本标签刚刚点过的赞。`,
    },
  },
  {
    id: 'missing',
    title: '后端有、页面没有的入口',
    foundation: '契约里有的路径，不代表这个前端已经接上。没接的能力不能从页面操作里推断它不存在。',
    apply: '领券、结账用券、热榜、商品搜索和上下架都没有页面。日销表没有读接口。个人页用的是订单表实时聚合，毛额含后来退掉的单。',
    code: {
      file: 'web/README.md',
      lines: '未接入',
      body: `// 未调用：/api/v1/coupons*、/api/v1/feed/hot
// 作者销售：GET /authors/{id}/sales 读订单表`,
    },
  },
]
