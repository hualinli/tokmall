# TokMall 面试讲解：一条业务链上的十个问题

> 这份文档的目标不是复述功能，而是按**面试官会追问的顺序**讲清楚：
> 遇到了什么问题、怎么定位、为什么这样解决、怎么证明解决了、边界在哪。
> 每个结论都能指回代码、测试或实验数据（文中给出路径）。
>
> 阅读建议：先看 §1 的一分钟版本和 §2 的业务闭环图，再挑故事看。

---

## 1. 一分钟版本

TokMall 是一个基于 CloudWeGo Hertz 的**短视频 Feed + 商城一体化后端**（模块化单体），
用 PostgreSQL、Redis、MinIO 三个依赖支撑一条完整链路：

> 作者上传发布视频 → 视频挂自己的在售商品 → 用户在 Feed 里刷到并下单 →
> 模拟支付 → 商家发货 → 买家确认收货 → 买家可申请退款、商家裁决 →
> 成交与退款通过 Transactional Outbox 分发到事件流 → 幂等消费形成作者日销投影 →
> 对账任务持续比对事实、流水、投影与事件，发现差异可重放或重建读模型。

技术上的三个着力点：

1. **正确性可以被证明**：超卖、幂等、状态机竞争、退款回补、事件不丢，
   每一项都有真实 PostgreSQL/Redis 上的并发实验或故障演练，而不是"我觉得没问题"。
2. **每一次技术引入都有触发条件**：缓存来自实测的 QPS 差距，限流来自被绕过的事实，
   Outbox 来自"提交后发消息会丢事件"的分析，而 Kafka 在压测后**被明确搁置**。
3. **简化都被显式记录**：`docs/architecture-decisions.md` 的 ADR、本文 §8 的边界清单，
   写清楚"现在没做什么、为什么、什么时候该做"。

规模：Go 代码约 2.3 万行（77 个源文件 / 45 个测试文件 / 287 个测试函数），
18 个版本化迁移，40 条 HTTP 路由，16 个包全部有测试，其中 9 个文件包含
显式开启的真实依赖集成测试。

---

## 2. 业务闭环与模块边界

```text
                    ┌──────────────── 内容域 ────────────────┐
  上传凭证(防伪造) → 视频(PENDING_REVIEW→PUBLISHED→DELETED) → Feed(游标分页/缓存)
                    └───────────────┬───────────────────────┘
                                    │ 挂商品(作者+在售校验, 上限5)
                    ┌───────────────▼───────────────────────┐
                    │ 商品域: 商品 / SKU / 库存(+幂等流水)   │
                    └───────────────┬───────────────────────┘
                                    │ 下单(单SKU, 条件扣库存)
                    ┌───────────────▼───────────────────────┐
                    │ 订单域: 支付 / 取消 / 超时关单 / 发货 / │
                    │        确认收货 / 退款申请与裁决        │
                    └───────┬───────────────────────┬───────┘
            同事务写 Outbox │                       │ 归属查询
                    ┌───────▼────────┐      ┌───────▼────────┐
                    │ 事件流(Redis   │      │ 归因: 作者成交  │
                    │ Streams+DLQ)   │      │ 统计(实时聚合)  │
                    └───────┬────────┘      └───────┬────────┘
                            │ 幂等消费(回执+投影同事务)      │
                    ┌───────▼────────────────────────▼───────┐
                    │ 作者日销投影(author_sales_daily)        │
                    └───────────────────┬────────────────────┘
                                        │ 六项检查 + 重放 + 重建
                              ┌─────────▼─────────┐
                              │ 对账 reconcile     │
                              └───────────────────┘
```

模块边界（`docs/backend-architecture.md`）：每个模块自己的 model 不出 service；
跨模块只读共享表时用各自的 read model 或直接 SQL，不共享类型。
`cmd/api/main.go` 是唯一的装配点，所有跨模块依赖都以**消费侧接口**注入
（例如 `feed` 定义 `FirstPageCache`，由 `main` 注入 Redis 实现）。

---

## 3. 故事一：越权发布与"不可见一律 404"

### 问题
视频发布最初只校验"视频存在且状态是 PENDING_REVIEW"，
于是**任何人拿到别人的 video_id 就能替别人发布**。同类问题还有：
删除视频对非作者返回 403（等于告诉攻击者"这个 ID 存在但不是你的"）、
未发布视频的详情能被他人读到。

### 排查
把每个内容域入口的守卫列成表：`publish`/`delete`/`get`/`play`/`like`/`follow`，
逐个问三个问题：谁可以调用？依据什么判断？失败返回什么？
发现三类缺陷：缺少 `author_id` 条件、错误码泄露存在性、状态判断与写入分离（先查后写）。

### 解决
- 所有状态迁移改成**带来源状态的条件更新**：
  `UPDATE videos SET status=? WHERE id=? AND author_id=? AND status=?`，
  受影响行数不为 1 即视为冲突（`internal/video/repository.go`）。
- 定义统一的可见性规则并写成表驱动状态机（`statusTransitions`）：
  DELETED 对所有人 404；未发布仅作者可见；不可见一律 `ErrNotFound`，
  **不泄露存在性**。
- 非作者删除视频从 403 改为 404（本次审计发现并修正，见 §7 故事八）。

### 证明
`internal/video/service_test.go` 覆盖越权发布/删除/状态冲突；
`cmd/api/main_test.go` 的路由同步测试保证端点没有漏挂认证中间件。

---

## 4. 故事二：Feed 性能演进与一次"故障放大器"

### 问题 A：1M 数据下首页 QPS 不够
百万视频数据下首页 QPS 6.6k，`db_pool_wait_count` 持续增长。

### 排查与解决
1. 先用 `EXPLAIN` 确认走了 `(published_at DESC, id DESC)` 的部分索引，不是全表扫描；
2. 引入 cache-aside + 首页缓存 + singleflight，QPS 6.6k → 11.3k，
   `db_pool_wait_count` 归零，瓶颈从数据库转移到 Redis 读；
3. 分页用**签名游标**而不是 offset：游标表示"上一页最后一条"的 `(published_at, id)`，
   HMAC 签名防篡改，`rank_version` 让排序策略变化时旧游标自动失效。

### 问题 B：缓存击穿保护反而放大故障
`singleflight` 把并发请求合并成一次查询，但闭包捕获的是**第一个请求的 context**。
当这个请求被客户端取消时，`loadPage` 带着已取消的 context 查库 → 报错，
而 singleflight 会把同一个错误广播给**所有等待者**：本来最多一个人失败，
变成整批请求 500。

### 解决
执行体改用不继承取消信号的独立超时上下文：

```go
loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), singleflightTimeout)
defer cancel()
v, err, _ := s.group.Do(firstPageKey, func() (any, error) { /* 用 loadCtx */ })
```

同时缓存返回值必须 copy-on-read（`cloneFeedResult`），否则请求级字段会被并发写。

### 证明
`internal/feed/service_test.go`（含数据竞争用例）+ `go test -race`；
压测数据见 `docs/performance-baseline.md`。

---

## 5. 故事三：库存、超卖与订单状态机

### 问题 A：怎么保证不超卖
"先查库存，再在应用里减"在并发下必然超卖：两个请求可能同时读到"还有 1 件"。

### 解决
把"检查"和"扣减"合并成一条 SQL，让数据库的行锁做仲裁：

```sql
UPDATE inventory SET quantity = quantity - $1, version = version + 1
WHERE sku_id = $2 AND quantity >= $1
```

`RowsAffected = 0` 就是库存不足。同时写一条 `ORDER_CREATED` 流水，
业务键 `(sku_id, reason, biz_id)` 唯一，保证"一个订单只扣一次"。

### 问题 B：超时订单还能支付
支付只校验状态机（PENDING_PAYMENT → PAID），不校验 `expires_at`。
关单 Worker 有吞吐上限、可能被关闭，而且它用 `FOR UPDATE SKIP LOCKED` 扫描——
**正在被支付事务锁住的行会被跳过**。于是出现"支付被拒 + Worker 也跳过"的窗口，
订单停在 PENDING_PAYMENT。

### 解决：支付路径的 lazy expiry
支付事务里自己判断过期，并顺手完成关单与库存回补：

```go
if !cmd.PaidAt.UTC().Before(order.ExpiresAt) {
    if err := expireLockedOrder(db, order, cmd.PaidAt.UTC()); err != nil { return err }
    expiredDuringPay = true
    return nil   // 注意：先提交副作用
}
// ...
if expiredDuringPay { return nil, ErrOrderExpired }  // 提交后再返回业务失败
```

这里踩过一个**事务语义的坑**：一开始直接在事务闭包里 `return ErrOrderExpired`，
结果 GORM 把整个事务回滚，刚写的关单和库存回补全被撤销（集成测试立刻抓到）。
正确做法是"事务内提交副作用并置位标记，提交后再返回业务错误"。

### 证明（`internal/order/postgres_integration_test.go`）
- 100 件库存、500 个并发买家、每人 1 件 → **恰好 100 成功、400 库存不足**，
  最终库存 0，订单/订单项/流水各 100 条，无半订单；
- 同用户同幂等键并发 100 次 → 只创建 1 个订单、只扣 1 件；
- 支付 vs 取消、支付 vs 过期、申请退款 vs 发货、申请退款 vs 确认收货 →
  每对竞争只有一个合法终态；
- 晚到支付（`expires_at` 已过）→ 拒绝且**顺手关单**，库存回补恰好一次。

---

## 6. 故事四：幂等不是一个 if，而是一套模式

项目里五处幂等，用的是同一套思路的三个变体：

| 场景 | 幂等键 | 冲突仲裁 | 语义 |
|---|---|---|---|
| 下单 | `(user_id, idempotency_key)` 唯一 + SHA256 请求指纹 | `ON CONFLICT DO NOTHING` + 重放读取 | 同键同载荷返回原订单；同键不同载荷 409 |
| 支付 | `payment_no` 与 `provider_trade_no` 双唯一 | 唯一约束 + `sameSuccessfulPayment` 比对 | 重复回调重放；改载荷 409 |
| 库存调整 | `(sku_id, reason, biz_id)` 唯一 | 唯一约束 + 载荷比对 | 重放返回当前库存；改载荷 409 |
| 点赞/关注 | 关系表主键/唯一键 | `ON CONFLICT DO NOTHING` | 结果语义固定（like → true） |
| 播放事件 | 客户端 `event_id`（UUID v7） | 主键冲突 + 业务指纹比对 | 同键同载荷成功；同键不同载荷 409 |

三个变体的差别只在"冲突后怎么办"：
**纯覆盖型**（点赞）直接成功；**资金/库存型**（下单、支付、库存）必须比对载荷；
**统计型**（播放）允许"服务端补时间"与"客户端给时间"区分开。

关键点：幂等不是"客户端重试不会出错"，而是**服务端对同一业务事实只承认一次**。
因此幂等键必须由服务端参与校验（指纹），否则改个数量重放就能套出第二单。

---

## 7. 故事五：售后（退款）闭环——状态机不能只有一个维度

### 问题
支付之后没有售后：用户付了钱、商家没发货，用户无法退款。

### 设计难点
退款是"申请 → 裁决 → 执行"三段式，天然有两个状态维度（履约 / 售后单据）。
如果只在退款表里记状态、订单状态不动，那么"退款中"的订单仍然可以被发货——
除非每条履约命令都去查一次退款表，而那又会引入"先查后写"的竞态。

### 解决：把售后状态投影进订单主状态
- 订单状态增加 `REFUND_REQUESTED` / `REFUNDED`，并用 `refund_from_status`
  记住申请前的状态（拒绝时退回）。
- 于是"退款中不可发货"由订单状态机**天然**保证，不需要额外查询。
- 退款单 `refunds` 用部分唯一索引保证"同一订单最多一笔未决或已成功退款"：

```sql
CREATE UNIQUE INDEX idx_refunds_order_active
  ON refunds (order_id) WHERE status IN ('REQUESTED', 'SUCCEEDED');
```

- 库存策略由业务语义决定：**未发货退款回补库存**（货还在仓库），
  **已发货退款不回补**（货已出库，退货入库是另一条流程）。
  回补流水仍用 `(sku_id, 'ORDER_REFUNDED', order_no)` 唯一兜底，只可能补一次。

### 一次真实的状态机缺陷（由集成测试抓到）
订单状态图里有 `REFUND_REQUESTED → FULFILLING` 这条边（拒绝退款时退回原状态），
而"商家发货"命令复用了同一张表，于是**商家可以在退款中直接发货，绕过裁决**。
数据库 CHECK 约束把它拦了下来（`orders_refund_columns_check`），
报错信息暴露了应用层漏洞。

修法是把"状态可达性"和"命令权限"拆开：

```go
var orderStatusTransitions = ...   // 完整状态图（含售后边，描述可达性）
var fulfillmentTransitions = ...   // 履约命令允许的迁移（没有退款边）
```

并加单测锁死"履约边必须是完整状态图的子集"，防止两张表漂移。

### 证明（`internal/order/refund_postgres_integration_test.go`）
50 个并发"同意退款"只有一次真正执行；已发货退款不回补库存；
拒绝后可再次申请且库存只补一次；申请退款与发货/确认收货竞争各自只有一个合法终态；
越权申请/裁决无副作用；退款终态后不能再支付/发货/二次裁决。

---

## 8. 故事六：可靠事件分发——Outbox、至少一次与幂等消费

### 问题
支付成功之后有一串副作用：作者成交统计、通知、风控、对账。
两种朴素做法都不成立：
- 放进支付事务 → 通知/统计是跨系统的，本地事务管不到，硬凑就是分布式事务；
- 提交订单后再发消息 → **进程在提交与发送之间崩溃就永久丢事件**。

### 解决：Transactional Outbox
事件当作业务数据，与订单状态在**同一事务**写入 `outbox_events`：

```go
// 支付事务内（internal/order/repository.go）
if err := outbox.EnqueueOrderPaid(db, outbox.OrderPaidPayload{...}); err != nil { return err }
```

中继（`internal/outbox/relay.go`）异步投递：
`SELECT ... WHERE status='PENDING' AND available_at <= now() ORDER BY id LIMIT n FOR UPDATE SKIP LOCKED`
→ 加租约 → 投递 → 标记 PUBLISHED；失败按 1s/2s/4s… 指数退避重试，
超过上限进 `DEAD`（DLQ），`cmd/outboxctl dlq requeue <event_id>` 人工重放。

投递语义是**至少一次**：先投递后标记，中途崩溃会重发同一条事件。
这不是缺陷而是选择——精确一次需要分布式事务或 broker 事务，
代价远高于让消费方做一次幂等去重。

### 幂等消费
消费者在**一个事务**里写"消费回执 + 业务投影"：

```go
tx.Transaction(func(tx *gorm.DB) error {
    inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
    if inserted.RowsAffected == 0 { duplicated = true; return nil }  // 重复投递
    return handler.Handle(ctx, tx, event)                            // 业务投影
})
// 提交成功后才 XACK
```

回执与投影同事务，所以**没有回执 ⟺ 投影没生效**——这个等价关系后来成了对账补偿的依据（§9）。
消费者崩溃时消息留在 Redis 的 PEL 里，`XAUTOCLAIM` 会认领重投；
投递次数超过上限进 DLQ Stream。

### 为什么默认 Redis Streams 而不是 Kafka（ADR-014）
- 本项目已经有 Redis；Streams 自带消费组、XACK、PEL、认领，足够支撑至少一次与崩溃重投；
- 单机学习项目的量级下，Kafka 的分区吞吐与保留策略是过剩能力；
- **代价要诚实记录**：Redis 持久化弱于 Kafka（AOF everysec 仍有秒级窗口），
  也没有跨机房复制。所以本项目不把"事件不丢"押在 broker 上，
  而是用 Outbox + 消费回执 + 对账重放兜底。
- 传输层是接口（`outbox.Publisher`），换 Kafka 只替换实现，事件语义与重放流程不变。

### 证明（`internal/outbox/integration_test.go`，真实 PostgreSQL + 真实 Redis）
1. 支付事件恰好投影一次；
2. 重复投递（同 event_id）不重复累加；
3. 退款事件冲减投影（毛额与退款分列）；
4. 业务事务回滚时事件一起消失（不会"业务没成功但下游收到通知"）；
5. 投递失败重试到上限进 DLQ，人工重放后台账恢复一致；
6. 消费者崩溃后未 ACK 的消息被认领并处理。

---

## 9. 故事七：对账——"你怎么知道数据是对的"

### 问题
上面所有机制都可能在极端情况下出错：手工改库、补偿脚本漏写流水、
broker 丢数据、消费者 bug。面试官会问：**你怎么知道现在数据是对的？**

### 解决：六项可执行检查（`internal/reconcile`）
每条检查都是一段能返回**差异行**的 SQL，报告直接给出"谁、差多少"：

| 检查 | 不变量 |
|---|---|
| `inventory_balance` | 库存余额 = 流水累计变更（守恒） |
| `order_stock_ledger` | 非待支付订单恰好一条下单流水，回补条数与终态匹配 |
| `refund_order_consistency` | 订单售后状态 ↔ 退款单 ↔ 支付金额三方一致 |
| `order_payment_consistency` | 已支付订单恰好一笔成功支付，未支付订单不得有 |
| `author_sales_projection` | 事件驱动投影 vs 订单事实（读模型漂移） |
| `outbox_delivery` | 已发布事件必须在宽限期内拿到消费回执 |

两条修复路径：
- **对账驱动重放**：把"已发布但没有消费回执"的事件放回 Outbox。
  因为回执与投影同事务，"没有回执"等价于"投影没生效"；重放沿用同一 `event_id`，
  消费侧仍会去重。
- **读模型重建**：投影可以从 `orders/refunds` 事实整体重建（`--rebuild`）。
  这是 CQRS 的关键性质——**读模型是可丢弃的物化视图**，因此"投影漂移"是
  可修复的运维问题，而不是数据损坏。

### 对账在真实数据上抓到的两件事（不是编的）
1. **口径不一致**：我最初写的检查用"净额"（排除已退款订单），
   而投影记的是"毛额 + 单独记退款"，第一次跑就报了 2 条差异。
   排查后确认是检查写错，于是把口径固化成规则：
   `order_count/units/gmv = 曾支付过的全部订单`，`refunded_* = 按退款发生日归集`。
   **对账本身也可能有 bug，所以口径必须写进注释和文档，而不是留在人脑里。**
2. **历史脏投影**：早期演练时退款事件还没接入，退款没有进投影。
   对账发现后，用 `--rebuild` 从事实重建，账实恢复一致。

### 证明（`internal/reconcile/integration_test.go`）
先造一份干净数据证明**不误报**，再逐项注入脏数据（改库存余额、改投影、
改订单状态、删消费回执）证明**能抓到**，最后证明重放/重建后恢复一致。

---

## 10. 故事八：限流为什么限不住（一个 P0 级安全发现）

### 问题
项目按 IP 做了限流（认证 100/min、读接口 6000/min）。
审计时发现：**这些限流可以被任意客户端逐请求绕过**。

### 排查
限流 key 取自 `ctx.ClientIP()`。Hertz v0.10.5 的默认可信代理列表是
`0.0.0.0/0` 与 `::/0`——也就是**信任所有人**，于是客户端自带的
`X-Forwarded-For` 会被原样采信。攻击者每个请求换一个 XFF 就是一个新的限流桶。

同一个审计还发现账号维度限流的 key 用请求体原始值（未 trim、未小写），
而业务查询会 `TrimSpace`：加一个前导空格就能换桶，10/min/账号形同虚设。

### 解决
```go
// 只信任配置显式声明的入口网段；未配置时不解析任何转发头
h.SetClientIPFunc(app.ClientIPWithOption(app.ClientIPOptions{
    RemoteIPHeaders: []string{"X-Forwarded-For", "X-Real-IP"},
    TrustedCIDRs:    cfg.TrustedProxies,   // TRUSTED_PROXIES，默认空
}))
```

并且配置层拒绝 `0.0.0.0/0` 这类"信任所有人"的取值（fail fast）；
账号 key 与业务查询共用同一个 `NormalizeAccountKey`（trim + 小写）。

### 这一组修复的清单（审计 → 修复，共 10 项 P0/P1）
| 编号 | 问题 | 修法 |
|---|---|---|
| BE-P0-1 | XFF 伪造绕过 IP 限流 | 显式可信代理 + 拒绝信任全网 |
| BE-P1-1 | 账号限流 key 未规范化 | 统一 `NormalizeAccountKey` |
| BE-P1-2 | 超时订单仍可支付 | 支付事务内 lazy expiry（§5） |
| BE-P1-3 | refresh 旋转是 check-then-act，并发双签发 | 黑名单改为 `Claim`（SETNX 抢占） |
| BE-P1-4 | singleflight 放大故障 | 独立超时上下文（§4） |
| BE-P1-5/6 | 超长 `session_id`/`trace_id` 打成 500，trace 可伪造 | 入口归一化，trace 只接受服务端 UUID |
| BE-P1-7 | 邮箱未规范化：312 字符能过校验却写库失败；大小写重复注册 | `NormalizeEmail` + 长度上限 + `lower(email)` 唯一索引 |
| BE-P1-8 | 取关把所有错误兜成 500 | 与 follow 对称的错误映射 |
| BE-P1-9 | 预签名 PUT 不绑定实际大小 | 记录为已知边界（§11），配额与 PostPolicy 列为后续 |

### 证明
`internal/middleware/ratelimit_test.go`（规范化）、
`internal/config/config_test.go`（可信代理与密钥必填）、
`internal/user/service_test.go`（并发刷新只有一个成功）、
`internal/order/postgres_integration_test.go`（晚到支付被拒）。

---

## 11. 故事九：一次真实的排查——"注册成功，登录 401"

这是我在端到端演练里踩到、并完整记录下来的 bug，因为它比任何设计都更像真实工作。

### 现象
`scripts/drill_commerce_loop.sh` 跑到第 2 步就停：注册返回 201，
紧接着用同一账号登录返回 401 `invalid credentials`。

### 排查过程
1. 手工复现：注册新用户 → 立刻用邮箱登录 → 401；用用户名登录 → 也 401。
2. 怀疑密码哈希：直接查库看 `password_hash` → 正常 bcrypt 串。
3. 查库看用户行：
   ```sql
   select id, username, email, status from users order by id desc limit 2;
   -- 14|probe009|probe9@example.com|      ← status 是空字符串！
   ```
4. 定位：为了让"停用账号不能登录"生效，我给 model 加了 `Status` 字段映射。
   **GORM 的 `Create` 会把结构体字段（含零值）一起写进 INSERT**，
   于是 `status` 被写成 `''`，绕过了列的 `DEFAULT 'ACTIVE'`。
   空状态既不是 ACTIVE 也不是 DISABLED，登录一律被判为"账号不可用"。

### 修复
- `Register` 显式写入 `ACTIVE`（主要防线，不依赖数据库默认值）；
- model 上加 `default:ACTIVE`，让 GORM 在零值时省略该列（第二道防线，
  防止将来新增的写入路径再踩同一个坑）；
- 迁移 000018 修复历史空值，并**恢复 `users_status_check` 约束**：
  000002 曾把状态枚举约束整体移到应用层，但 `status` 与 `role` 不同——
  写错的后果是认证失效，必须由数据库兜底；
- 新增真实 PostgreSQL 集成测试，断言"库里存的 status 是 ACTIVE"
  并且"注册后能立刻登录"。

### 这个 bug 教了什么
**fake repository 永远发现不了"ORM 与 DDL 默认值打架"这类问题。**
单元测试里 fake 会把 Status 填成 ACTIVE，测试全绿；只有真实数据库能暴露它。
这也是本项目坚持"关键不变量必须有真实依赖集成测试"的原因。

---

## 12. 故事十：性能与容量——数据驱动的技术引入

`docs/performance-baseline.md` 记录了四个时点的实测（含历史条件与命令）：

| 场景 | 结果 | 结论 |
|---|---|---|
| Feed 1M 数据，无缓存 | 6.6k QPS，db_pool_wait 持续增长 | 瓶颈在数据库读 |
| Feed 1M 数据，首页缓存 + singleflight | 11.3k QPS，db_pool_wait 归零 | 瓶颈转移到 Redis 读 |
| play 写 + feed 读混合 | feed 只降 6.5%（6,249→5,840） | **"写拖慢读"证伪**，Kafka 异步化搁置 |
| 下单+支付 50 并发 20s | 18,228 请求 0 失败，911 req/s，p50 39ms / p99 237ms | 库存扣减与成功订单数完全一致；Outbox 无积压 |

最后一条的观察值得展开：**p50 39ms 不是数据库慢，而是"每单两次串行事务 +
25 条连接池竞争"**。若要推到数千 QPS，先做的是提高连接池、合并往返，
而不是引入消息队列。这就是"先测量，再引入技术"的具体体现。

---

## 13. 工程化：让结论不可绕过

| 手段 | 内容 | 位置 |
|---|---|---|
| 测试分层 | 单元（fake，快）→ 真实依赖集成（显式开关）→ e2e（Playwright） | `internal/**/*_test.go`、`web/e2e/` |
| 契约同步 | 路由 ↔ OpenAPI 双向 diff；**schema ↔ Go DTO 字段/类型/必填对齐**（17 个 DTO，已做变异验证） | `cmd/api/main_test.go`、`internal/api/spec_test.go` |
| 并发证明 | 超卖、幂等、状态竞争、退款回补、关单竞争 | `internal/order/postgres_integration_test.go` |
| 事件语义证明 | 恰好一次投影、重复投递、DLQ 重放、崩溃认领 | `internal/outbox/integration_test.go` |
| 对账证明 | 干净不误报 + 四类脏数据能抓到 + 重放/重建恢复 | `internal/reconcile/integration_test.go` |
| 故障演练 | Redis 停机、中继停摆、事件丢失、消费崩溃 | `scripts/drill_failure_modes.sh` |
| 端到端演练 | 内容→商品→下单→支付→事件→投影→退款，curl+psql 全程可见 | `scripts/drill_commerce_loop.sh` |
| CI | 后端（vet/单测/竞态/集成）、契约、前端（漂移/类型/lint/单测/构建）、e2e | `.github/workflows/ci.yml` |
| 可观测性 | HTTP 指标 + 连接池 + 缓存命中 + Outbox 积压/死信 + 消费延迟 + 对账差异 | `internal/observability/`、`/metrics` |
| 运维入口 | `outboxctl status/dlq list/requeue/drain`、`reconcile [--replay] [--rebuild]` | `cmd/outboxctl`、`cmd/reconcile` |

---

## 14. 高频问答速查

**Q：为什么是模块化单体，不是微服务？**
业务规模不需要独立扩缩容，而单体让"下单事务"保持本地事务（简单且强一致）。
模块边界按未来拆分设计：模块间不共享类型、只通过消费侧接口依赖，
拆服务时按模块边界切。ADR-001 记录了取舍。

**Q：缓存一致性怎么保证？**
cache-aside：先写库、再删缓存，TTL 兜底；singleflight 防击穿；
返回值 copy-on-read 防并发写；关注流缓存按 follower 精确失效。
已知边界：视频删除后关注流缓存最多脏 60s（TTL），记录在 ADR 与边界清单里。

**Q：为什么不用分布式事务/Seata？**
本项目只有"支付 + 事件"需要跨边界一致，用本地事务 + Outbox 就够（ADR-016）。
分布式事务解决的是"多个数据库同时提交"，代价是协调者、悬挂、空回滚等复杂度，
在单库 + 事件驱动的场景下是负收益。

**Q：消息会丢吗？重复消费怎么办？**
会（broker 可能丢），所以不假设它不丢：Outbox 保证事件先落库，
消费回执 + 投影同事务保证幂等，对账 `outbox_delivery` 发现"已发布无回执"
并重放。重复消费由 `consumed_events` 主键去重。

**Q：怎么防止超卖？**
条件更新 `WHERE quantity >= n` + 数据库行锁，不写"先查后减"。
证据：100 库存 / 500 并发的实验恰好 100 成功。

**Q：怎么保证接口幂等？**
见 §6 的五处幂等表：唯一约束 + 请求指纹，冲突后按业务语义返回原结果或 409。

**Q：线上出问题怎么排查？**
结构化日志（slog，含 request_id/user_id/order_id/event_id）+ Prometheus 指标
（HTTP、连接池、缓存、Outbox、消费、对账）+ 三个可观测的运维命令。
演练脚本让"依赖挂了会怎样"可复现。

**Q：这套东西的边界在哪？**
见 §15。

---

## 15. 已知边界（诚实清单）

| 边界 | 现状 | 触发条件（什么时候该做） |
|---|---|---|
| 支付渠道 | 模拟支付，不是渠道回调；无签名/验签/对账文件 | 接入真实渠道时必须独立入口 + 验签 + 幂等 |
| 退款 | 只支持全额退款；商家同意即视为渠道退款成功；不做退货物流 | 支持部分退款/退货入库时新增状态与库存流程 |
| 履约 | 模拟发货，无物流轨迹；COMPLETED 后不接受退款 | 接入物流或 7 天无理由时扩展 |
| 事件传输 | 默认 Redis Streams（AOF everysec，仍有秒级丢失窗口） | 需要跨机房/更高吞吐时换 Kafka（接口已抽象） |
| 上传大小 | 预签名 PUT 不绑定实际大小，无配额 | 生产必须换 PostPolicy 的 content-length-range + 配额 |
| 关注 Feed 缓存 | 视频删除后 follower 缓存最多脏 60s | 需要强一致时按 follower 集合失效或版本号 |
| 库存写入路径 | order/product/inventory 三处直接写 `inventory` 表，靠 DB 约束兜底 | 库存逻辑变复杂时收口为单一写入口 |
| 观测 | 指标齐全但无告警规则、无链路追踪（Tracing） | 有真实 SLO 需求时接 Alertmanager/OTel |
| 部署 | 单机 docker compose，无 k8s/灰度/回滚流水线 | 需要多环境发布时补 CD |
| 秒杀 | 未做 Redis 预扣/一人一单 | 热点库存行压测出现可测瓶颈后（ADR-019） |

---

## 15.5 从同类开源项目借鉴了什么（DomTok 对比）

我完整读过 west2-online/DomTok（字节青训营第一名的分布式电商后端：Kitex 微服务 +
RocketMQ + Kafka + ES + OTel 全家桶），对比结论与落地清单见
[`docs/domtok-comparison-and-borrowing.md`](domtok-comparison-and-borrowing.md)。
这里只讲面试时最值得说的三件事：

**（1）借了它的延迟消息，但把事实源放回数据库。**
DomTok 用 RocketMQ 的 `DelayLevel` 做订单超时（10 分钟后回滚库存），思路是对的，
但它把消息当成了事实源：broker 单主 ASYNC_FLUSH、消息没有幂等键、DLQ 无人消费——
丢一条消息就是一笔订单的库存永久泄漏。TokMall 用 Outbox 的 `available_at`
实现同样的延迟语义（不新增任何组件），消费者回查订单状态并做条件更新，
**消息全丢系统仍在轮询间隔内收敛**（ADR-021）。这是"引入中间件不等于提升可靠性"的具体例子。

**（2）借了它的布隆过滤器思路，但补上了它缺的三件事。**
DomTok 的网关布隆过滤器是纯内存态、启动不预热（漏判会让封禁用户继续访问）。
TokMall 的实现（`internal/existence`）补上：双缓冲轮换（删除无法从过滤器移除）、
warmup 保护（装载完成前一律放行，空过滤器会把真实数据判成不存在）、
以及"只对记录不存在写负缓存"（可见性拒绝不能写，否则作者看不到自己的视频）。
实测：不存在的 ID 压测从 2,996 req/s（每次都查库）提升到 5,086 req/s 且数据库零查询（ADR-022）。

**（3）它的交易内核一条都没抄。**
它的"预扣库存"不校验库存（只判 `lock_stock < 0`，真正的校验在支付之后）、
支付状态存在 Redis 里当判据（写路径有 bug，导致已支付订单被延迟消息误回滚）、
金额用 float64 贯穿全链路（`int64(9.99) == 9`）、订单状态更新没有 `WHERE status=from`。
这四条恰好是 TokMall 用条件更新 + CHECK 约束 + Outbox + 对账已经解决的问题——
**读同类项目的价值，一半在于知道哪些不能抄。**

顺带借过来的工程细节：GORM 的 `ParameterizedQueries`（SQL 日志不带参数值）+
`IgnoreRecordNotFoundError` + 慢查询阈值（穿透压测下日志 I/O 曾是瓶颈，改完吞吐提升 10 倍）；
CI 的 `go mod tidy` 漂移检查、"自动修复后比 diff"、CodeQL 与 govulncheck。

## 16. 现场演示动线（10 分钟）

面试时不要从"我做了哪些功能"开始，而是**按一条链走一遍，每一步都给一个可验证的结论**。
下面这套命令在本机跑过（前置：`make dev`）。

| 时间 | 动作 | 你要说的话 |
|---|---|---|
| 0:00 | `make drill` | "这是完整业务闭环：内容 → 商品 → 下单 → 支付 → 事件 → 投影 → 退款。注意第 7 步：`outbox_published=1 pending=0`，事件与订单同事务提交后立刻被投递、被幂等消费；第 8 步库存从 4 回到 5，退款回补只发生一次。" |
| 2:00 | `make reconcile` | "这是六项对账：库存守恒、订单流水、退款一致、支付一致、读模型漂移、事件回执。现在全 OK——重点不是它现在绿，而是它能抓到什么。" |
| 3:00 | `make drill-failure` | "故障演练：停掉 Redis，支付仍然成功、事件留在 Outbox；恢复后自动补投；再造一条'下游从未收到'的事件，对账报差异并重放修复。**我们不假设 broker 不丢消息。**" |
| 5:00 | `TOKMALL_RUN_PG_INTEGRATION=1 ... go test ./internal/order/ -run TestPostgres -v` | "100 件库存、500 个并发买家：恰好 100 成功、400 库存不足、最终库存 0、订单/订单项/流水各 100 条。这条不变量是用真实数据库证明的，不是靠 code review。" |
| 6:30 | `TOKMALL_RUN_REDIS_INTEGRATION=1 ... go test ./internal/outbox/ -v` | "六个事件场景：恰好一次投影、重复投递不累加、退款冲减、事务回滚不留事件、DLQ 重放、消费者崩溃后认领。" |
| 8:00 | `make loadgen CONCURRENCY=50 DURATION=20s` | "50 并发 20 秒：18,228 请求零失败，911 req/s，p99 237ms；结束后库存扣减数与成功订单数完全一致。瓶颈是连接池和每单两次串行事务，不是数据库慢——所以我没有引入消息队列。" |
| 9:00 | 打开 `docs/interview-walkthrough.md` §15 | "这是我明确知道并写下来的边界：模拟支付、只做全额退款、预签名上传未绑定大小、关注流缓存最多脏 60s……每条都写了什么时候该做。" |

三条纪律：
1. **先给结论再给过程**："这里的关键不变量是 X，我用 Y 证明了它"比"我用了 A、B、C 技术"更有说服力。
2. **主动暴露一个失败**：§11 的"注册成功但登录 401"是最好的素材——
   它展示了排查方法（查库、定位 ORM 与 DDL 默认值冲突、加三层防线、补真实依赖测试），
   而不是"我写的代码一次就对"。
3. **每个技术引入都要有触发条件**：缓存来自 6.6k→11.3k 的实测差距，
   限流来自被绕过的事实，Outbox 来自"提交后发消息会丢事件"的分析，
   而 Kafka 在压测后**被否决**——这比"我用了 Kafka"更能体现判断力。

## 17. 简历要点（可直接改写）

- 设计并实现短视频 Feed + 商城一体化后端（Go/Hertz/PostgreSQL/Redis/MinIO），
  覆盖内容发布、商品与库存、下单支付、履约、售后、归因与对账的完整闭环；
- **一致性**：用条件更新 + 行锁实现防超卖，真实 PostgreSQL 并发实验证明
  "100 库存 / 500 并发恰好 100 成功"；订单/支付/退款状态机用条件更新 + 数据库
  CHECK 双层兜底，四组竞争场景均只有一个合法终态；
- **可靠事件**：实现 Transactional Outbox + 至少一次投递 + 幂等消费（回执与投影
  同事务）+ DLQ 与人工重放，6 个集成场景证明"不丢、不重、可恢复"；
- **可证明性**：实现六项对账检查（库存守恒、订单流水、退款一致、支付一致、
  读模型漂移、事件回执）+ 对账驱动重放 + 读模型重建，真实数据上抓到并修复
  两处不一致；
- **性能**：Feed 首页缓存 + singleflight 把 1M 数据下的 QPS 从 6.6k 提升到 11.3k
  并把数据库等待归零；压测证明"写不拖慢读"，据此**否决**了引入 Kafka 的方案；
  下单链路 50 并发 911 req/s、p99 237ms 且零失败；
- **安全**：审计并修复限流绕过（伪造 X-Forwarded-For）、并发 refresh 双签发、
  超时订单可支付等 10 项 P0/P1 问题，全部有回归测试。
