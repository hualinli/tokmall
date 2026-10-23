# 后端架构

本文回答"系统怎么实现、为什么这样选、代价是什么"。每个主题按 **做法 → 为什么 → 代价与边界** 组织。业务规则见 [业务](business.md)，运行和恢复操作见 [运维](operations.md)，测量数据见 [性能](performance.md)。

事实以代码为准：装配看 [main.go](../cmd/api/main.go)，配置看 [config.go](../internal/config/config.go)，接口看 [OpenAPI](../internal/api/openapi.yaml)，表结构看 [migrations](../migrations/)（当前到 000026）。

<a id="monolith"></a>

## 总体结构

TokMall 是共享 PostgreSQL 的模块化单体。`cmd/api` 是组合根：加载配置、连接 PG 和 Redis、执行迁移、创建 RustFS 客户端，装配各模块并注册路由，同时在同一进程里启动后台任务（对象清理、媒体探测、热榜物化、存在性重建、订单到期、自动确认收货、Outbox 中继与消费、周期对账）。收到退出信号后先关 HTTP，再逆序停 worker。

| 依赖 | 职责 |
|---|---|
| PostgreSQL | 所有业务事实：交易、状态机、幂等约束、Outbox、消费回执、投影、删除任务。表结构只由 golang-migrate 管理，禁止 AutoMigrate |
| Redis | Feed 缓存与版本号、负缓存、限流、refresh token 抢占、热榜分钟桶与窗口、默认的 Streams 事件传输。不保存交易事实 |
| RustFS | S3 兼容的私有媒体存储。浏览器用预签名 URL 直传，服务端负责封存、探测、清理 |

**分层。** handler 处理 HTTP DTO、读取认证身份、把业务错误映射为响应；service 执行业务规则，对外只暴露领域参数和视图；repository 负责 SQL、事务，并把 ORM 错误翻译成领域哨兵错误。错误用 `%w` 包装、`errors.Is/As` 判断；业务拒绝不按系统错误记日志，未知错误返回 500。见 [统一响应](../internal/response/response.go)。

**模块协作。** 模块不共享持久化 model。跨模块读用本模块的 read model 或显式 SQL；需要调用时由消费方定义接口、`main.go` 装配。少量窄域直接调用：订单调用 `coupon.DiscountFor`，各模块通过 Outbox API 写事件。

**为什么是单体。** 订单、库存、券、Outbox 在同一个 PG 事务里提交，交易不变量由事务和数据库约束直接守住，不需要分布式事务。

**代价。** 所有模块共享数据库和进程的故障域；库存写入分散在 order/product/inventory 三处，靠数据库约束兜底。PG 与 Redis/RustFS 之间没有原子性，需要缓存失效、事件幂等和对象清理补偿。拆分服务的前提是出现独立部署、团队边界或容量隔离需求，并有测量证明收益。

**教学入口的取舍。** 教程复用前端 `/learn`，以独立场景的分步模型说明交易语义，避免学习操作向业务库写入演示订单；学习记录留在浏览器，不增加业务表或 API。代价是模型需随实现维护，不能证明数据库锁和回滚正确，也不提供跨设备进度同步；验证仍依赖真实集成测试。入口与使用范围见[前端说明](../web/README.md)。

### 模块与数据归属

| 模块 | 职责 | 拥有的表或读模型 |
|---|---|---|
| `user` | 注册、登录、refresh 轮换、登出、本人资料 | `users`；Redis refresh 抢占记录 |
| `video` | 上传票证、封存、状态与可见性、播放事件、媒体探测、对象清理 | `videos`、`upload_tickets`、`play_events`、`object_delete_jobs` |
| `interaction` | 点赞、关注、社交列表 | `likes`、`follows` |
| `feed` | 全局/关注 Feed、游标、缓存、商品卡装配、热榜 | 只读模型，不拥有事实 |
| `product` / `inventory` | 商品、SKU、上下架；库存调整与流水 | `products`、`skus`、`inventory`、`inventory_transactions` |
| `promotion` | 视频挂载商品 | `video_products` |
| `order` | 下单、支付、取消/超时、履约、退款 | `orders`、`order_items`、`payments`、`refunds` |
| `coupon` | 券模板、领券、满减规则 | `coupons`、`user_coupons` |
| `outbox` | 中继、Streams 消费、回执、DLQ | `outbox_events`、`consumed_events` |
| `attribution` | 作者成交实时统计、日销投影 | `author_sales_daily` |
| `reconcile` | 九项对账、事件重放、投影重建 | 跨模块只读 |
| `breaker` / `existence` / `tracing` | 存储熔断、负缓存、traceparent | 横切能力 |

<a id="commerce"></a>

## 交易

### 下单事务

[订单仓储](../internal/order/repository.go) 在一个事务里按固定顺序执行：

1. **幂等锁。** 按 `(user_id, idempotency_key)` 取 `pg_advisory_xact_lock`，再查已有订单。请求指纹包括 SKU、数量、来源、券；指纹相同返回原单，不同则冲突。唯一约束加 `INSERT ... ON CONFLICT` 兜底。
2. **读取可售商品。** 对商品和 SKU 加共享锁，读稳定的价格和商家；有来源时校验 `video_products` 里的有效关系。
3. **锁券。** 用券时对持券行加更新锁，检查归属、同商家、未使用、有效期、门槛，用 `coupon.DiscountFor` 算优惠。
4. **准备写入。** 插入订单、订单项快照、库存流水，把券从 UNUSED 条件更新为 USED，写入 `available_at = expires_at` 的超时延迟事件。
5. **最后扣库存。** `UPDATE ... SET quantity = quantity - ? WHERE quantity >= ?`，按影响行数判断，成功即提交。

**为什么先锁幂等键。** 如果先锁券，同键重试会先撞上"券已被首单占用"而失败，拿不到原订单。

**为什么库存放最后。** 库存行是热点，放在事务尾部能缩短成功路径的持锁时间；10 月 12 日的实验里热点吞吐中位数从单轮参考 355.7 提高到 597.5 单/秒，见 [性能](performance.md#latest-results)。代价是售罄时会多做一些最终回滚的准备写入。

**金额。** 全部用整数分，乘法前检查溢出。数据库 CHECK 保证 `total = original - discount` 且实付大于零；每单唯一订单项的约束守住"单 SKU"。买家不能提交价格、优惠额或商家 ID。

<a id="order-expiry"></a>

### 支付、取消与超时

支付是受认证保护的买家模拟命令，直接写入成功支付事实，不对接渠道。订单行 `FOR UPDATE`；支付单号和模拟渠道交易号是幂等身份，相同载荷重放，换了订单或金额则冲突；金额必须等于实付。接真实渠道时需要单独的验签回调入口、主动查单和渠道对账，不能把这个命令改名当回调用。

若支付到达时已过期，事务先把订单改为 EXPIRED、回补库存、退券并**提交**，提交后才返回"已过期"错误。如果在事务闭包里直接返回错误，关单副作用会一起回滚。

关单有三条路径：订单超时延迟事件（加速）、5 秒轮询 worker（兜底）、支付时 lazy expiry。订单行锁、`WHERE status = 'PENDING_PAYMENT'` 条件更新、库存流水唯一键共同保证只回补一次。数据库订单状态是唯一事实源。见 [超时处理](../internal/order/timeout_handler.go)、[轮询](../internal/order/expiry_worker.go)。

**待证明：** 延迟事件和轮询做的是同一件事。如果只靠轮询，关单延迟已经满足需求，延迟事件就是多余的复杂度。见 [性能计划](performance.md#plan)。

### 履约与退款

履约使用独立的 `fulfillmentTransitions`：商家只能把自己的订单从 PAID 推到 FULFILLING，买家只能把自己的订单从 FULFILLING 推到 COMPLETED。不能复用完整状态图，否则退款的"恢复原状态"边会被误当成发货许可。重复同一动作返回重放结果。

**自动确认收货。**
- **截止时间。** 发货时由 service 按 `ORDER_AUTO_COMPLETE_AFTER`（默认 7 天）算出 `auto_complete_at`，与 `fulfilling_at` 同时写入；之后只会顺延、不会清空。[000025](../migrations/000025_order_auto_complete.up.sql) 的 CHECK 约束保证两者成对出现，且截止时间晚于发货时间。
- **扫描。** [AutoCompleteWorker](../internal/order/auto_complete_worker.go) 每分钟扫一次部分索引 `WHERE status = 'FULFILLING'`，用一条 `UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)` 批量完成到期订单：跳过正被买家锁住的行，外层再加 status 条件兜底。买家申请退款与 worker 竞争时谁先提交谁生效，晚到的退款申请看到 COMPLETED 会被拒绝。
- **退款被拒后顺延。** 拒绝退款退回 FULFILLING 时，截止时间顺延到 `max(原截止时间, 拒绝时刻 + ORDER_AUTO_COMPLETE_REJECT_GRACE)`（默认 3 天），否则申请期间截止时间已过，拒绝的下一刻就会被自动确认。
- **只用轮询的原因。** 截止时间可变，已发出的延迟事件撤不回；而且分钟级精度就够了。确认收货没有库存、券或事件副作用，所以不写 Outbox。
- **证据。** 见 [PG 测试](../internal/order/auto_complete_postgres_integration_test.go)。

**退款处理时限。** 申请时由 service 按 `REFUND_AUTO_APPROVE_AFTER`（默认 48 小时）算出 `respond_by` 并写入退款单。被拒绝后再次申请是一张新退款单，截止时间重新计算。[000026](../migrations/000026_refund_respond_deadline.up.sql) 用 `CASE` 约束来源和裁决人：商家裁决必须有 `decided_by`，超时同意必须为空且只能是成功。

[RefundAutoApproveWorker](../internal/order/refund_auto_approve_worker.go) 每分钟调用一次 [ApproveOverdueRefund](../internal/order/refund_repository.go)。每个事务只处理一笔：`FOR UPDATE OF o SKIP LOCKED` 先锁订单行，再构造来源为 TIMEOUT 的裁决交给 `approveRefund`。因此回补库存、退券和 OrderRefunded 事件与商家同意是同一条路径。锁顺序和商家裁决一致，避免一边锁退款单、一边锁订单。商家正握着订单锁时这一笔被跳过；商家晚到时，同意按重放返回，拒绝得到已经裁决。不用一条 UPDATE 批量同意，是因为同意要做的事不只有改状态。证据：[PG 测试](../internal/order/refund_auto_approve_postgres_integration_test.go)。

[退款仓储](../internal/order/refund_repository.go) 的申请和裁决都先锁订单行。部分唯一索引保证同一订单最多一笔 REQUESTED 或 SUCCEEDED 退款，被拒的退款单保留为历史。同意时，退款事实、订单 REFUNDED、库存回补（仅原状态为 PAID 时）、退券和 OrderRefunded 事件同事务提交。拒绝时按订单上的 `refund_from_status` 恢复。

### 库存

库存调整先按 `(sku_id, reason, biz_id)` 插入唯一流水作为幂等声明，再条件更新余额；同一业务键换载荷则冲突。余额有非负 CHECK，并应等于流水之和（由对账检查）。见 [库存仓储](../internal/inventory/repository.go)、[000011](../migrations/000011_inventory_idempotency_and_invariants.up.sql)。

**代价与演进条件。** 热点 SKU 仍串行争同一行锁，等锁的事务会占着连接。只有固定到达率压测确认热点锁是瓶颈、且入口保护和缩短事务都不够时，才考虑 Redis 预扣或排队，以及随之而来的补偿成本。

### 优惠券

领券事务先锁模板行，检查有效期，条件递增发行量，再检查每人限领并创建持券；数据库 CHECK 限制发行量。下单时锁持券、校验后条件核销。

退券在券仍有效时改回 UNUSED、已过期改为 EXPIRED，并清空持券上的 `order_id/used_at`；订单保留 `user_coupon_id` 作为历史。迁移 [000021](../migrations/000021_coupon_reuse.up.sql)/[000024](../migrations/000024_closed_order_coupon.up.sql) 的部分唯一索引只约束仍占券的订单，所以退回的券可以再用。见 [券规则](../internal/coupon/service.go)、[券仓储](../internal/coupon/repository.go)。

### 推广与成交统计

推广只允许作者挂自己的在售商品，挂载时视频必须已发布，[000013](../migrations/000013_enforce_video_product_active_limit.up.sql) 限制每条视频最多 5 个有效商品。下单只检查关系仍 ACTIVE，不复查视频状态，见 [业务](business.md#成交统计)。

作者统计接口在 REPEATABLE READ 只读事务里直接聚合订单，保证总计和拆分来自同一快照，见 [实时统计](../internal/attribution/repository.go)。另有 Outbox 消费者把支付毛额记到支付日、退款额记到退款日，写入 `author_sales_daily`，见 [投影](../internal/attribution/projector.go)。

**待解决：** 投影表目前没有读者。要么让统计接口改读投影，并用压测证明大订单量下实时聚合变慢；要么承认它只是学习用的读模型。见 [性能计划](performance.md#plan)。

<a id="events"></a>

## 事件：Outbox 与 Redis Streams

**做法。**

- **生产。** 业务事实和 Outbox 事件在同一个 PG 事务写入（`Enqueue` 只接受 tx）。当前事件只有三种：OrderPaid、OrderRefunded、OrderTimeout（延迟）。
- **中继。** Relay 领取到期事件，带租约发送到 Redis Stream，失败退避重试，超过上限进入 PG 内的 DEAD。
- **消费。** 消费者在一个事务里写 `(consumer, event_id)` 回执和投影，提交后才 XACK；重复事件因回执冲突不重复生效。投递是至少一次，不宣称恰好一次。
- **PEL 恢复。** 分页 XPENDING 加 XCLAIM（Redis 内再次检查 MinIdle）。扫描位置在内存中推进、到末尾绕回；投递次数和消息来自同一页，次数查询失败就不认领。超过投递次数进入消费侧 DLQ Stream，和 PG 内 DEAD 是两个位置。
- **保留与背压。** 按所有必需消费组的确认水位安全裁剪 Stream；容量满时背压，未发送的事件留在 PG。早期固定 `MAXLEN ~ 10000` 会裁掉未消费消息，这是 10 月 11 日定位的缺口，见 [诊断](../benchmarks/2026-10-11/diagnosis/README.md)。
- **连接预算。** 开启消费者时为它保留独立的 5 条连接，前台 20，总上限 25，防止前台等锁时耗尽连接、消费者拿不到连接。

实现见 [中继](../internal/outbox/relay.go)、[传输](../internal/outbox/transport.go)、[消费者](../internal/outbox/consumer.go)。

**为什么用 Streams 而不是 Kafka。** 复用已有 Redis，就获得消费组、确认、PEL 恢复；当前规模不需要分区和长期保留。

**代价。**
- Redis 持久化故障可能丢已发布消息，要靠 PG 事件、回执和对账重放恢复。
- 停用的消费组会阻止回收。背压只把积压留在 PG，不保证长期收敛。
- 消费者串行逐条提交，是当前的吞吐短板。

**直接消费。** `OUTBOX_TRANSPORT=postgres` 时不启动中继和 Streams 消费者，由 [DirectConsumer](../internal/outbox/direct_consumer.go) 用 `FOR UPDATE SKIP LOCKED` 取到期事件，回执和投影仍在同一个事务里。默认仍是 `redis`。对照排空耗时见 [性能](performance.md#streams-vs-postgres)。

<a id="recovery"></a>

## 对账与恢复

九项检查：`inventory_balance`、`order_stock_ledger`、`refund_order_consistency`、`order_payment_consistency`、`author_sales_projection`、`coupon_issuance`、`coupon_usage`、`order_amount_split`、`outbox_delivery`。对账只报告差异，不自动修改订单、支付、库存、券。见 [对账](../internal/reconcile/repository.go)、[CLI](../cmd/reconcile/main.go)。

**重放。** 超过宽限期、已发布但缺回执的事件，以原 `event_id` 放回 Outbox，靠回执去重。

**重建日销投影。** 必须同时满足：
- 指定实际消费组；
- 支付和退款事件（不论 PENDING/PUBLISHED/DEAD）都有该组回执，未来才到期的 timeout 不算；
- 同一事务用 `SHARE ROW EXCLUSIVE NOWAIT` 锁住订单、明细、退款、Outbox、回执、投影表。

争锁或缺回执就拒绝。原因是一次真实反例：支付已提交但事件未消费时重建，先把 200 计入投影，旧事件随后到达又加 200，GMV 翻倍。见 [门禁测试](../internal/reconcile/rebuild_integration_test.go)。

**代价。** 重建需要停写的维护窗口；不能删除或伪造回执来绕过门禁。对账任务在 API 进程里每 5 分钟跑一次全表聚合，订单量增大后对前台的干扰尚未测量。

<a id="feed"></a>

## Feed

### 时间流与游标

全局 Feed 按 `(published_at, id)` 倒序，`id` 保证同一时间也有稳定全序；多取一条判断 `has_more`。游标表示上一页末项，带排序版本并用 HMAC 签名（Base64 只是编码）。见 [cursor.go](../internal/feed/cursor.go)、[repository.go](../internal/feed/repository.go)。

关注 Feed 是拉模式，读关注作者的完整时间线：

| 关注数 | 查询方式 |
|---|---|
| 0 | 直接返回 |
| 1～32 | 每个作者按索引取 limit+1 条，再归并 |
| 更多 | 全局时间索引过滤关注关系 |

32 来自合成数据的执行计划对比，不是通用常数。生产只允许 `TimeRanker`；实验用的 `HotRanker` 排序键和时间游标不一致，配置或 service 注入都会被拒绝。推拉混合、inbox 扇出都没有实现，只有关注规模的测量证明拉模式受限时才考虑。

<a id="cache"></a>

### 缓存

**缓存范围。** 只缓存默认 limit、无游标的首页（全局和关注各一套），缓存内容是视频主体。深页游标分散，缓存所有组合会增加 key 数量和失效成本。

**读路径。** Cache-Aside + singleflight + 回源前二次检查。回源用独立超时上下文，避免首个请求被取消时连累其他等待者；共享结果在装配个性化字段前先复制。

**两级缓存。** 可选的分片 LRU（L1，默认 5 秒）位于 Redis（L2，默认 60 秒）之前，L1 TTL 不得超过 L2。

**写后失效。**
- 数据库提交后先删本地 L1、再删 L2，成功后递增 `feed:version`，其他实例每秒探测一次版本。
- 全局首页回填会记下回源前的 Redis 版本，写入时版本已变就放弃；L2 里版本不符的页面也按未命中处理。
- 关注首页按用户失效，加上 TTL 收敛；发布、删除不遍历粉丝 key。

**动态字段。** 点赞数和商品卡不进缓存，每次请求按页合并成一条 SQL 读取；点赞和 SKU 价格先分别聚合再连接，避免 JOIN 把计数放大，也避免 N+1。见 [tiered_cache.go](../internal/feed/tiered_cache.go)、[dynamic_read.go](../internal/feed/dynamic_read.go)。

**代价：缓存是最终一致，不是强一致。** 以下路径仍会读到旧首页：
- L2 删除失败时会跳过版本递增；
- 版本递增失败，或版本读取失败时沿用旧值；
- L1 在探测间隔内仍用本地版本；
- 关注首页的在途回填可能盖住一次删除。

因此不能把探测间隔或 L1 TTL 当作所有故障下的陈旧上界。缓存命中仍要访问数据库读动态字段，这正是 Feed 的主要瓶颈，见 [性能](performance.md#latest-results)。

### 热榜

`GET /api/v1/feed/hot` 是独立的物化热榜。点赞、播放先累加到 Redis 分钟桶，后台每分钟物化一个冻结窗口：key 为 v2，最多 2000 条，TTL 10 分钟。游标包含 `window_end`、`hot_score`、`video_id`；窗口不重写，过期窗口的游标被拒绝。

读取时先取排名条目，再批量查仍已发布的视频，不可见项跳过；游标按本轮扫描的最后一个条目推进，所以一页可能少于 limit。首页窗口为空或 Redis 故障时降级为时间流，并在后续页继续用时间游标；已经在用热榜游标的请求遇到故障直接返回错误，不中途换排序。见 [hot_cache.go](../internal/feed/hot_cache.go)、[hot_service.go](../internal/feed/hot_service.go)。

<a id="existence"></a>

## 存在性判定（防穿透）

[existence.Guard](../internal/existence/guard.go) 只使用 Redis 负缓存：

- 负缓存只记录真实的数据库 miss，带 TTL 抖动；不记录"存在但不可见"的未发布视频，否则会误伤作者本人。
- 负缓存读取失败时放行到数据库，不把依赖故障伪装成 404。
- 没有命中负缓存的请求继续查数据库。新建的视频和商品不需要登记到本地集合。

本地布隆过滤器已删除。它的阴性不能证明其他实例没有刚创建这条记录，所以从不参与判定，却要常驻内存并每 10 分钟全表重建。

<a id="media"></a>

## 媒体：上传、校验与清理

**上传与封存。**
- 预签名 PUT 只允许写 `staging/<object_key>`，签名绑定 Content-Length 和 Content-Type。视频 1 MiB～512 MiB，图片 1 KiB～5 MiB。
- 封存时再检查对象实际大小，按源 ETag 条件复制到最终 key，防止检查之后对象被替换。
- 票证在预签名成功后才落库。核销时锁票证行；票证核销与视频创建在同一事务里。

见 [video/service.go](../internal/video/service.go)、[storage.go](../internal/storage/storage.go)。

**媒体校验。** worker 下载对象到临时目录，用 ffprobe 检查时长、尺寸、编码，必要时用 ffmpeg remux 启用 faststart。下载等暂时失败会推迟重试；已删除视频不会被取件。多个 worker 可能重复探测，但条件写回只接受一次结果，回写时持视频行锁，不会复活已删除视频。见 [probe_worker.go](../internal/video/probe_worker.go)、[000022](../migrations/000022_probe_publish_gate.up.sql)。

**删除与清理。** 删除状态和 `object_delete_jobs` 同事务写入，先尝试同步删除，失败由 worker 每 10 秒重试。staging 对象要等原 PUT 有效期结束才清理；过期未用的票证也会清理。见 [cleanup_worker.go](../internal/video/cleanup_worker.go)、[000023](../migrations/000023_object_cleanup.up.sql)。

**代价。**
- 锁内的存储调用会占用数据库连接。
- 复制成功但 PG 回滚会留下无引用的最终对象，目前没有孤儿回收。
- 物理删除前，已签发的 GET URL 仍可访问。
- 没有用户级的累计上传配额。

<a id="auth"></a>

## 认证、限流与可见性

**JWT 与 refresh。** access JWT 短时有效，只验签，不逐请求检查账户状态或撤销；登录和 refresh 时检查账户可用性。refresh 按 jti 用 Redis `SET NX` 原子抢占，只有抢到的请求能签发新 token，TTL 等于旧 token 剩余寿命；抢占后签发失败需要重新登录。logout 对无效 token 幂等成功。见 [user/service.go](../internal/user/service.go)、[blacklist.go](../internal/user/blacklist.go)。

**浏览器凭据。** token 存在 localStorage，多标签页用 BroadcastChannel/storage 事件同步。这挡不住同源 XSS，也不能仲裁多个标签页同时刷新；HttpOnly Cookie + CSRF 改造尚未实现。见 [auth-store.ts](../web/src/lib/auth-store.ts)、[auth-sync.ts](../web/src/lib/auth-sync.ts)。

**限流。** Redis 固定窗口 Lua 脚本：读请求按 IP，受保护写请求按用户，认证接口另有 IP 配额；登录和注册还按"规范化账号的 SHA-256"计配额，邮箱和用户名分开计。Redis 故障时放行。请求体上限 64 KiB，认证接口 16 KiB。`TRUSTED_PROXIES` 默认为空（不信任转发头），禁止配置全网段。见 [ratelimit.go](../internal/middleware/ratelimit.go)。

**资源归属。** 一律在服务端校验，身份只来自认证中间件。视频状态迁移由表驱动，数据库 `WHERE status = from` 最终防并发。

<a id="observability"></a>

## 配置与可观测性

- **密钥。** `JWT_SECRET` 和 `FEED_CURSOR_SECRET` 在所有环境必填；生产环境还校验密钥长度和互不相同、DB/Redis/RustFS TLS、拒绝占位凭证，见 `Validate`。
- **日志。** slog 输出到 stdout 和轮转文件；ContextHandler 注入 request_id、trace_id、span_id、路由模板、用户 ID，并按字段名脱敏。
- **追踪。** 只实现 W3C traceparent 的校验、生成和透传，没有 OTel SDK 和导出器。
- **指标。** Prometheus `/metrics` 默认监听 `127.0.0.1:9091`，启动时主动探测；指标标签用路由模板，禁止 user_id 等高基数字段。告警规则在 [tokmall.yml](../observability/prometheus/rules/tokmall.yml)，没有部署 Alertmanager。
- **熔断。** 对象存储走 closed/open/half-open 熔断装饰器，预签名和对象操作共用一个熔断器；对象不存在、大小无效是业务结果，不计故障；open 时返回 503。存储不可用不阻塞启动。SDK 缓存了 bucket region 时会本地签名，所以签名成功不代表存储可用。见 [breaker/storage.go](../internal/breaker/storage.go)。
- **健康检查。** `/health/ready` 只 ping PostgreSQL。

## 明确不做

单进程部署，后台任务不是独立服务。Kafka、支付渠道、部分退款与退货入库、商品范围券、Feed 推送扇出、推荐系统都没有接入。PG 与 Redis/RustFS 之间没有分布式事务，靠 Outbox、幂等、补偿任务和对账处理已知失败窗口。
