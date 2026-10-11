# Commerce 当前技术实现

> 更新于 2026-10-07。以下以当前工作区源码、OpenAPI 契约和 migrations/000001–000024 为准；本文保留会影响正确性的事务与不变量，不重复列完整 DDL 或端点清单。路由契约见 [OpenAPI](../internal/api/openapi.yaml)。

## 模块边界

这是共享 PostgreSQL 的模块化单体。product 管商品/SKU，inventory 管库存与流水，order 维护交易事实和订单状态，coupon 管券规则，promotion 管视频商品关系，attribution 提供作者统计，outbox/reconcile 负责异步投影和校验。模块以消费侧接口或自己的只读 SQL 投影协作，不共享其他模块的持久化 model。商品、订单和券的归属都来自认证上下文中的用户 ID；当前没有独立 merchants 表。入口分别见 [商品](../internal/product/)、[库存](../internal/inventory/)、[订单](../internal/order/)、[优惠券](../internal/coupon/)、[推广](../internal/promotion/)、[归因](../internal/attribution/)。

## 订单状态与事务

~~~text
PENDING_PAYMENT ──▶ PAID ──▶ FULFILLING ──▶ COMPLETED
       │              │             │
       ├─▶ CANCELED   └─▶ REFUND_REQUESTED ◀─┘
       └─▶ EXPIRED                 │
                              ┌────┴────┐
                              ▼         ▼
                         REFUNDED   申请前状态
~~~

订单状态图见 [order/service.go](../internal/order/service.go)，数据库约束随 [迁移](../migrations/) 演进。COMPLETED 是履约终态，不接受退款；退款申请只允许从 PAID 或 FULFILLING 进入。REFUND_REQUESTED 会把订单移出履约命令允许的状态；拒绝时依据订单上保存的 refund_from_status 回到 PAID 或 FULFILLING。状态图定义“可达边”，履约命令另有自己的状态守卫，不能复用完整状态图。

### 创建与幂等

[订单仓储](../internal/order/repository.go)在一个 PostgreSQL 事务内完成订单创建：

1. 事务开头按 user_id 与 idempotency_key 获取 pg_advisory_xact_lock，再查已存在订单。请求指纹包括 SKU、数量、来源和可选持券 ID；相同载荷重放原订单，不同载荷返回冲突。唯一约束和 INSERT ... ON CONFLICT 继续兜底。
2. 锁定可售商品和 SKU 以读取稳定价格与归属；若有视频来源，再验证该视频商品关系仍有效。订单项保存商品标题、规格、单价、商家和数量快照。
3. 若使用优惠券，先锁持券行并检查用户归属、同商家、未使用、有效期和门槛，再计算折扣。幂等事务锁之后依次是商品/SKU 共享锁、可选来源关系共享锁、可选持券行更新锁、订单唯一键 claim，最后才条件更新库存。
4. 插入订单、ORDER_CREATED 流水和单条订单项，再将持券状态从 UNUSED 条件更新为 USED，关联订单；同事务写入到期时间等于 expires_at 的延迟 Outbox 事件。这些准备写入在提交前不可被其他事务读取。
5. 最后按 quantity >= 请求数量条件扣减库存，然后直接提交，缩短热点库存行锁持有时间。任何库存不足、券状态竞争或写入失败都会回滚订单、明细、流水、券和事件；不是把它们拆成独立提交。

v1 的单 SKU 边界由数据库对每单唯一 order_item 约束，不能只靠 handler 约定。服务端以整数分计算 original_amount，优惠券规则由 [coupon.DiscountFor](../internal/coupon/service.go) 计算；支付金额满足 total_amount = original_amount - discount_amount 且必须大于零，数据库 CHECK 再兜底。买家不能提交价格、折扣额或商家 ID。

### 支付、取消与超时

当前支付端点是受认证保护的买家模拟命令：它直接写入成功支付事实，不会调用支付渠道。订单行先以 FOR UPDATE 锁定；支付单号与模拟渠道交易号是幂等身份，完全相同的成功支付重放为 replay，不同订单、金额或身份冲突。金额必须等于订单实付额。真实渠道接入必须另加回调验签、时间窗和商户金额校验，不能把此命令改名为渠道回调。

若支付请求到达时 expires_at 已过，事务会先将待支付订单改为 EXPIRED、回补库存与库存流水、释放持券，然后提交；提交后才向调用者返回订单过期错误。这样业务失败不会撤销关单副作用。买家取消和过期 Worker 也只对 PENDING_PAYMENT 做条件状态更新；订单行锁、WHERE status 条件和库存流水唯一键共同防止支付、取消、超时并发时重复回补。到期 Outbox 事件用于加速处理，5 秒轮询 Worker 是安全网；数据库订单状态才是事实源。实现见 [订单仓储](../internal/order/repository.go)、[超时事件处理](../internal/order/timeout_handler.go) 和 [轮询 Worker](../internal/order/expiry_worker.go)，决定见 [ADR-021](architecture-decisions.md)。

### 履约与退款

商家只能将自己订单从 PAID 推进到 FULFILLING；买家只能将自己的订单从 FULFILLING 推进到 COMPLETED。归属过滤、订单行锁和条件更新共同仲裁并发操作；重复同一履约动作返回重放结果，其他非法转换拒绝。

[退款仓储](../internal/order/refund_repository.go)的申请和裁决都先锁订单行，再校验状态、改退款单与订单状态。数据库部分唯一索引确保同一订单最多一笔 REQUESTED 或 SUCCEEDED 退款；被拒绝的退款保留历史单据，之后允许重新申请。商家同意即代表模拟退款成功，金额固定等于订单 total_amount，资金事实、订单 REFUNDED、库存/券处理和 OrderRefunded Outbox 事件同事务提交。已发货的订单不回补库存；退货质检和重新入库不在实现范围。订单完成后不接受售后申请。全额退款取舍见 [ADR-015](architecture-decisions.md)。

## 库存、优惠券与历史金额

库存调整在事务里先按 (sku_id, reason, biz_id) 插入唯一流水作为幂等 claim，再条件更新数量；库存不足时两者一起回滚，重复 key 必须匹配原业务载荷，否则冲突。下单通过带数量下界的原子扣减防超卖。取消/超时和未发货退款按订单数量回补；已发货退款不回补。库存余额应等于库存流水变更之和，具体写入见 [库存仓储](../internal/inventory/repository.go)，数据库幂等约束见 [000011](../migrations/000011_inventory_idempotency_and_invariants.up.sql)。

优惠券仅有商家满减券。领取事务先锁券模板行，再检查有效期和领取上限、条件递增发行量、检查每人限领并创建持券；数据库 CHECK 限制发行量。使用时必须匹配订单商家，不能跨商家；规则检查未使用状态、有效期和商品金额门槛。一张券不能同时被两张仍占用的订单核销。并发策略和规则见 [券仓储](../internal/coupon/repository.go)、[订单用券](../internal/order/repository.go) 与 [ADR-024](architecture-decisions.md)。

取消、超时和成功退款会释放券：在券仍有效时改回 UNUSED，过期时改为 EXPIRED，并清空持券记录上的 order_id/used_at。订单本身保留 user_coupon_id，订单项也保留成交快照，作为历史交易记录；迁移 000021/000024 的部分唯一索引允许关闭或退款订单留下此引用，同时让持券可在之后再次使用。退款回券与订单终态在同一事务内提交，失败时一起回滚。金额拆分和重用约束见 [000019](../migrations/000019_coupon.up.sql)、[000021](../migrations/000021_coupon_reuse.up.sql) 与 [000024](../migrations/000024_closed_order_coupon.up.sql)。

## 推广、归因与对账恢复

推广关系只允许视频作者挂自己的在售商品，挂载时视频必须已发布；数据库约束限制每条视频最多 5 个有效商品。下单来源由客户端提交，订单仓储只检查 video_products 中仍为 ACTIVE 的关系及商品 ID，没有重新检查视频当前是否仍为 PUBLISHED，也无法证明买家真的从该视频点击进入。因此来源快照适合当前演示统计，不能作为结算或反作弊凭据；封禁/删除视频后仍 ACTIVE 的旧关系也可能被直接提交归因。商品下架不会改写历史订单，商品卡变为不可购买。实现见 [推广仓储](../internal/promotion/repository.go)、[订单来源校验](../internal/order/repository.go) 和 [000013](../migrations/000013_enforce_video_product_active_limit.up.sql)。

作者销售接口在 REPEATABLE READ 只读事务中直接聚合订单事实，保证总计和视频/商品拆分来自同一快照。它按已成功支付的订单计算订单数、去重买家、销量和实付 GMV；之后成功退款的订单仍留在毛额里，退款金额单独统计，所以净额由调用方按 GMV 减退款额计算。另有 Outbox 消费者把支付毛额记在支付日、退款金额记在退款日，写入 author_sales_daily 投影；该表不替代当前销售接口的实时查询。实现见 [实时统计](../internal/attribution/repository.go) 和 [日销投影](../internal/attribution/projector.go)，口径取舍见 [ADR-017](architecture-decisions.md)。

支付成功、退款成功及下单超时事件与对应业务事实同事务写入 Outbox。消费者在一个数据库事务里提交事件回执与投影，提交后才 XACK；重复事件按 event_id 去重，失联消费者由 XAUTOCLAIM 接管，超过投递上限进入 DLQ。当前传输使用 Redis Streams，Kafka 未接入。实现见 [Outbox 仓储](../internal/outbox/repository.go) 和 [消费者](../internal/outbox/consumer.go)，传输决策见 [ADR-014](architecture-decisions.md)。

当前 reconcile 定义九项检查：库存余额与流水守恒、订单库存流水、退款单与订单/支付一致、订单与支付一致、作者日销投影、券发行量、券核销、订单金额拆分、Outbox 消费回执。对账只报告差异，不自动改订单、支付、库存或券等事实数据。它能将超过宽限期且已发布但没有消费回执的事件，以原 event_id 放回 Outbox；相同 ID 保持消费幂等。确认交易事实可信后，可以从订单与退款事实重建 author_sales_daily。DLQ 需由运维显式重放。恢复只覆盖事件投递和作者读模型，不能代替资金、库存或券的人工核查。实现见 [对账](../internal/reconcile/repository.go)、[对账服务](../internal/reconcile/service.go) 和 [reconcile 命令](../cmd/reconcile/)。

## 明确边界

当前不接真实收款或真实退款渠道，不提供物流、退货入库、部分退款、COMPLETED 后退款、购物车、多 SKU 订单、跨商家订单、跨商家推广、佣金结算或秒杀。优惠券前端购买流程与热榜前端调用也尚未实现。未来范围见 [路线图](roadmap.md)；模块取舍集中记录在 [架构决策](architecture-decisions.md)。
