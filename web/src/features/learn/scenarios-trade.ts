import type { Scenario } from './scenario'

export const commerceScenarios: Scenario[] = [
  {
    id: 'price',
    title: '价格以 SKU 行为准',
    foundation: '金额如果采信客户端，请求可以被改便宜。下单当时的价格要抄进订单，之后改价不影响旧单。',
    apply: '新增 SKU 的价格以分为单位且必须为正。订单明细保存标题、规格、单价和商家。创建 SKU 时同步初始化库存行，缺行不当成一次成功入库。',
    code: {
      file: 'internal/product/service.go',
      lines: '226–228',
      body: `func (s *Service) AddSKU(...) (*SKUItem, error) {
    if p.ProductID <= 0 || p.MerchantID <= 0 || p.PriceCents <= 0 {
        return nil, ErrInvalidParam
    }`,
    },
  },
  {
    id: 'offsale',
    title: '下架还能打开详情，但不能买',
    foundation: '历史订单和分享链接还会指向下架商品。详情可以留下，可购买的规格不能再列出来。',
    apply: '删除对外是 404。下架返回商品信息，SKU 列表为空。别人的商品和不存在是同一个 404。封面必须是这个用户自己的图片票。',
    code: {
      file: 'internal/product/service.go',
      lines: '210–212',
      body: `if product.Status != ProductStatusOnSale {
    return &ProductDetail{ProductItem: *item, SKUs: []SKUItem{}}, nil
}`,
    },
  },
  {
    id: 'stock',
    title: '补货先占流水，再改余额',
    foundation: '余额不能为负。同一业务键重复提交应该看到原来的结果，而不是再加一次。库存不足时这次 claim 也不该留下，否则换数量就无法重试。',
    apply: '商家补货走 inventory.Adjust。先按 (sku, reason, biz_id) 插入流水，冲突则视为已经做过。然后要求 quantity + change >= 0。下单扣减不走这个函数，写在订单事务末尾。',
    code: {
      file: 'internal/inventory/repository.go',
      lines: '107–108',
      body: `Where("sku_id = ? AND quantity + ? >= 0", tx.SkuID, tx.Change).
    Updates(map[string]any{"quantity": gorm.Expr("quantity + ?", tx.Change)})`,
    },
  },
  {
    id: 'discount',
    title: '折扣只有一个函数',
    foundation: '门槛、有效期和面额如果在下单、展示、对账各算一次，三个数字会对不上。实付被优惠吃到零以下时，订单不该成立。',
    apply: 'DiscountFor 要求券未使用、当前时间在有效期内、整单原价达到门槛。数据库还有实付 = 原价 − 优惠的 CHECK。券只能用于该商家自己的 SKU。',
    code: {
      file: 'internal/coupon/service.go',
      lines: '144–158',
      body: `if uc.Status != UserCouponUnused { return 0, ErrUserCouponUsed }
if orderAmount < c.ThresholdAmount { return 0, ErrThresholdNotMet }
if discount > orderAmount { discount = orderAmount }
return discount, nil`,
    },
  },
  {
    id: 'claim',
    title: '领取锁住模板再加发行量',
    foundation: '发行量和每人限领要在同一个事务里判断。超限时发行量的增加必须跟着撤销。过期券不能再领。',
    apply: '锁模板行，条件更新 issued_quantity，再数这个用户领了几张。发行量写成 0 时领取条件把它当成不设上限。列表上的已过期按当前时间算，没有定时任务刷状态。',
    code: {
      file: 'internal/coupon/repository.go',
      lines: '106–116',
      body: `Where("id = ? AND (total_quantity = 0 OR issued_quantity < total_quantity)", c.ID).
    Updates(map[string]any{"issued_quantity": gorm.Expr("issued_quantity + 1")})
if claimed.RowsAffected != 1 { return ErrSoldOut }`,
    },
  },
  {
    id: 'restore',
    title: '关闭订单之后券可以再用',
    foundation: '占着券的订单应该唯一。订单关闭后如果唯一索引仍把历史引用算占用，同一张券就再也用不了。退回时还要看券是否已经过期。',
    apply: '取消、支付超时、退款成功会退券。仍在有效期内回到 UNUSED，过期则标成 EXPIRED。订单保留 user_coupon_id。唯一索引只约束还占着券的订单。',
    code: {
      file: 'internal/order/refund_repository.go',
      lines: '455–457',
      body: `// restoreUserCoupon：券仍在有效期内 → UNUSED；已过期 → EXPIRED。
// 退券与订单终态同事务提交。`,
    },
  },
  {
    id: 'link',
    title: '一条视频最多挂五个在售商品',
    foundation: '挂载要同时满足作者、发布状态和商品在售。上限放在约束里，应用层数错也不会超挂。摘掉关系不该要求商品仍然在售。',
    apply: '视频已发布且作者是你，商品在售且商家也是你，才能挂上。超出 5 个由 video_products_active_limit_check 拒绝。下架或视频不可再挂之后，原作者仍能解除关系。',
    code: {
      file: 'internal/promotion/repository.go',
      lines: '119–125',
      body: `WHERE v.id = ? AND v.author_id = ? AND v.status = 'PUBLISHED'
  AND p.merchant_id = ? AND p.status = 'ON_SALE'
FOR SHARE OF v, p`,
    },
  },
]

export const orderScenarios: Scenario[] = [
  {
    id: 'fingerprint',
    title: '幂等键和指纹各管一件事',
    foundation: '重试必须能拿回同一次尝试的结果。同键换成另一件商品如果也返回原订单，调用方会以为换货已经生效。',
    apply: '先对用户和幂等键加事务级咨询锁。指纹是用户、SKU、数量、来源和券的哈希，不含幂等键本身。指纹相同重放原单，不同则冲突。唯一约束是第二道。',
    code: {
      file: 'internal/order/service.go',
      lines: '358–369',
      body: `type fingerprintPayload struct {
    UserID int64; SKUID int64; Quantity int32
    Source *Source; UserCouponID *int64
}`,
    },
  },
  {
    id: 'tail',
    title: '库存放在事务最后',
    foundation: '热点行持有越久，别的事务等得越久。锁的顺序还要全库一致，否则和另一条路径形成等待环。',
    apply: '顺序是咨询锁、读 SKU、锁券、插订单和流水、改券、写超时事件，最后按 quantity >= 购买数量扣库存。失败则前面全部回滚。锁顺序固定为先券、后库存。',
    code: {
      file: 'internal/order/repository.go',
      lines: '171–176',
      body: `// 独立写入已准备好，再竞争热点库存行。
stock := db.Model(&inventoryRow{}).
    Where("sku_id = ? AND quantity >= ?", cmd.SKUID, cmd.Quantity)`,
    },
  },
  {
    id: 'graph',
    title: '发货和确认只用窄表',
    foundation: '「状态之间能不能到达」和「这条命令能不能触发」不是同一张表。完整图里的退回边如果被发货命令使用，退款还没裁决就能被发成待收货。',
    apply: '履约只允许已支付到待收货、待收货到已完成。支付写下成功支付和 OrderPaid，不动库存。来源视频由客户端提交，服务端核对商品确实挂在这条已发布视频上。',
    code: {
      file: 'internal/order/service.go',
      lines: '172–175',
      body: `var fulfillmentTransitions = map[Status]map[Status]struct{}{
    StatusPaid:       {StatusFulfilling: {}},
    StatusFulfilling: {StatusCompleted: {}},
}`,
    },
  },
  {
    id: 'pay',
    title: '支付身份对上但金额不同就是冲突',
    foundation: '支付重试要认得出是不是同一笔。单号相同、金额或订单不同，不能当成成功重放。',
    apply: 'payment_no 和渠道交易号都唯一。两者与订单、金额一致才重放。金额必须等于订单实付。实付被券吃到 0 或以下时，下单直接拒绝。',
    code: {
      file: 'internal/order/repository.go',
      lines: '225–228',
      body: `if existing != nil {
    if !sameSuccessfulPayment(*existing, order.ID, cmd) {
        return ErrIdempotencyConflict
    }`,
    },
  },
  {
    id: 'expiry',
    title: '过期的支付先关单再返回',
    foundation: '在事务闭包里直接返回错误，会把闭包里刚写的补救一起滚掉。调用方看到失败，库里的关单也不在。',
    apply: '支付发现已过 expires_at 时，先在事务里关单、回补、退券并提交，再返回已过期。另两条路是到点的 Outbox 事件，以及大约每 5 秒的 SKIP LOCKED 扫描。事实以数据库里仍待支付且已到期为准。',
    code: {
      file: 'internal/order/repository.go',
      lines: '206–208',
      body: `// 不能直接 return ErrOrderExpired。
// 事务闭包返回错误会回滚刚刚写下的关单与库存回补。`,
    },
  },
  {
    id: 'cancel',
    title: '取消只作用于待支付',
    foundation: '已经付款的订单不能靠取消把库存悄悄加回去。取消和超时是同一类关单：货还在，钱还没付，券也该退回。',
    apply: '买家取消时按 user_id 锁订单。状态必须仍是待支付。回补流水原因是 ORDER_CANCELED，并退券。别人的订单和不存在是同一个错误。',
    code: {
      file: 'internal/order/repository.go',
      lines: '304–309',
      body: `Where("id = ? AND user_id = ?", orderID, userID).Take(&order)
// 仅 PENDING_PAYMENT 可以取消，回补原因 ORDER_CANCELED`,
    },
  },
  {
    id: 'complete',
    title: '自动确认扫描仍待收货的行',
    foundation: '截止时间会被改写时，不适合只发一条收不回的延迟消息。多个 worker 领行要用跳过已锁行的方式，避免和正在退款的事务抢同一把锁。',
    apply: '发货写下 auto_complete_at，默认 7 天后。worker 把仍是待收货且已到期的订单收成完成。这一步不退券、不写事件。买家随后再点确认按重放处理。',
    code: {
      file: 'internal/order/read_lifecycle.go',
      lines: '268–274',
      body: `UPDATE orders SET status = COMPLETED ...
WHERE status = FULFILLING AND id IN (
    SELECT id FROM orders
    WHERE status = FULFILLING AND auto_complete_at <= ?
    FOR UPDATE SKIP LOCKED)`,
    },
  },
]

export const refundScenarios: Scenario[] = [
  {
    id: 'who',
    title: '只有已支付和待收货能申请',
    foundation: '完成后还允许退款，就会和「交易已经结束」冲突。申请期间订单必须离开可发货、可确认的状态。',
    apply: '全额等于实付。同一订单最多一笔申请中或已成功的退款，拒绝之后可以再申请。申请时记下申请前状态，并写入 respond_by，默认 48 小时。',
    code: {
      file: 'internal/order/refund_service.go',
      lines: '180–182',
      body: `func refundableFrom(status Status) bool {
    return status == StatusPaid || status == StatusFulfilling
}`,
    },
  },
  {
    id: 'reject',
    title: '拒绝回到申请前的状态',
    foundation: '拒绝时再猜该回哪里，会把未发货的单子推进成待收货。恢复目标必须是申请当时记下的状态。',
    apply: '申请前是已支付，拒绝后仍是已支付，可以发货。申请前是待收货，拒绝后仍是待收货。自动确认时间只顺延：剩余不足 3 天就补到拒绝后 3 天，更长就保持原截止。',
    code: {
      file: 'internal/order/refund_repository.go',
      lines: '384–398',
      body: `restored := *order.RefundFromStatus
if restored == StatusFulfilling {
    deadline := *order.AutoCompleteAt
    if deadline.Before(minimum) { deadline = minimum }
    changes["auto_complete_at"] = deadline
}`,
    },
  },
  {
    id: 'stock',
    title: '已发货的同意不把库存加回去',
    foundation: '退款成功不等于货物回到仓库。未发出的货可以回补；已经发出的货不能冒充入库。',
    apply: '同意时看申请前状态。来自已支付才回补库存。来自待收货不回补。两种同意都退券，并写 OrderRefunded。',
    code: {
      file: 'internal/order/refund_repository.go',
      lines: '327–331',
      body: `// 未发货退款才回补库存。已发货退款不回补。
if order.RefundFromStatus != nil && *order.RefundFromStatus == StatusPaid {
    if err := restoreStock(db, order, at); err != nil {
        return err
    }
}`,
    },
  },
  {
    id: 'timeout',
    title: '超时同意和商家走同一条路径',
    foundation: '到期自动处理如果另写一套同意，库存、退券和事件会和人工同意分叉。来源要能区分是人还是系统。',
    apply: 'worker 锁订单，锁内再核对状态和 respond_by，然后调用 approveRefund。来源 TIMEOUT，decided_by 为空。数据库要求商家裁决必须有操作者，超时裁决必须为空且结果是成功。',
    code: {
      file: 'internal/order/refund_repository.go',
      lines: '155–160',
      body: `// 先锁订单行，再把 TIMEOUT 裁决交给 approveRefund。
// 只锁 orders，不在这条 SELECT 里锁 refunds。`,
    },
  },
]

export const raceScenarios: Scenario[] = [
  {
    id: 'branch',
    title: '后拿到锁的人看到已提交的状态',
    foundation: '两条命令抢同一行时，先提交的改变事实，后进入的按新事实判断自己还能不能做。',
    apply: '退款申请把订单移出已支付。发货命令只认识窄表，读到退款处理中就失败。自动确认同样只扫描仍是待收货的行。',
    code: {
      file: 'internal/order/service.go',
      lines: '150–152',
      body: `// 履约命令必须使用 fulfillmentTransitions。
// 否则 REFUND_REQUESTED → FULFILLING 会被发货误用。`,
    },
  },
  {
    id: 'rollback',
    title: '最后一步失败，前面的写入一起消失',
    foundation: '事务的意义是这次尝试要么全部可见，要么全部不可见。半张订单比失败更难修。',
    apply: '库存条件不满足时，订单、流水、占券和超时事件回滚。支付过期若在闭包里直接返回错误，关单和回补也会被撤销。正确做法是先提交关单，再返回已过期。',
    code: {
      file: 'internal/order/repository.go',
      lines: '184',
      body: `if stock.RowsAffected == 0 {
    // 条件扣减失败，整个事务回滚
}`,
    },
  },
  {
    id: 'deadlock',
    title: '两把锁的顺序必须全库相同',
    foundation: 'A 握着甲等乙，B 握着乙等甲，两边都不释放，就是死锁。避免的办法是所有路径按同一个顺序申请锁。',
    apply: '下单和退券相关路径都先券、后库存。「顺序相反」那一帧是故意展示被避免的环。超时任务如果先锁退款单，会和已经锁着订单的商家互相等；实际它只锁订单，锁被占用时跳过。',
    code: {
      file: 'internal/order/repository.go',
      lines: '68',
      body: `// 锁在扣库存之前，且所有路径都用同一顺序（券 → 库存），避免死锁。`,
    },
  },
]
