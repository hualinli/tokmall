# TokMall 面试讲解：四条能落到证据的工程故事

> 更新于 2026-10-09。按“问题 → 机制 → 不变量 → 证据 → 边界”讲，不把架构名词当成结论。
> 本文讲当前代码；实现选择统一看 [架构决策记录](architecture-decisions.md)，业务范围看
> [Commerce 说明](commerce-business.md) 与 [技术设计](commerce-v1-technical-design.md)。

## 一分钟介绍

TokMall 是 Go / Hertz 的短视频 Feed + 商城模块化单体。作者上传视频并挂自己的在售商品，
用户从 Feed 进入下单；订单、库存、退款和作者销售投影由 PostgreSQL 事务与约束守住本地不变量。
Transactional Outbox 通过 Redis Streams 至少一次投递，消费回执与投影同事务提交；九项对账检查
可以发现事实、流水、券、金额、投影和事件回执之间的差异，并支持事件重放或投影重建。

我会重点讲四件事：订单并发、缓存与游标、Outbox 与对账、媒体封存与删除补偿。
证据来自对应源码和显式开启的真实依赖集成测试。它是一个学习型项目，不代表已经满足生产上线的
容量、灾备或第三方支付要求。

## 1. 订单并发：把“检查后扣减”变成一个数据库事实

### 问题

“先读库存，再在应用里减”允许两个并发请求同时看到最后一件。仅靠应用状态机也无法解决支付、
取消、超时关单之间的竞争；重复提交还可能重复扣库存。

### 做法

- 创建订单时服务端读取 SKU 与价格；同一个 PostgreSQL 事务里检查并占用优惠券、条件扣库存、
  写订单快照、订单项、库存流水和延迟超时 Outbox 事件。
- 扣减使用条件更新，只有 **quantity >= 请求数量** 才减库存；影响行数决定成功还是库存不足。
  **(user_id, idempotency_key)** 唯一约束负责并发仲裁，服务端请求指纹区分“同键同载荷重放”
  与“同键改载荷”，后者返回冲突。
- 支付、取消与到期关单锁定订单行并按当前状态迁移。支付路径自己检查 **expires_at** 并执行
  lazy expiry；延迟事件只是加速器，轮询 Worker 是安全网，订单表仍是事实源。
- 取消、到期和退款成功会释放占用券；过期券回到 **EXPIRED**。退款状态进入订单主状态，履约命令
  使用独立迁移表，避免“拒绝退款时可回到履约状态”被发货命令误用。

这个状态边界曾经出过问题：完整状态图含有“退款申请被拒后回到原履约状态”的边，发货命令复用完整图，
因此应用层允许在退款申请中发货。数据库 CHECK 拒绝了写入，集成测试据此暴露应用层漏洞；现在发货只用
履约专属迁移表，并有测试保证它是完整状态图的子集。

### 证据与取舍

显式开启的真实 PostgreSQL 测试用 500 个并发买家竞争 100 件库存，断言恰好 100 单成功、
400 单因库存不足失败，最后库存为零，订单、订单项和流水数量相符；同用户同幂等键并发重试只扣一次。
支付与取消、支付与到期、退款与履约也有真实 PostgreSQL 竞争测试。证据见
[订单并发集成测试](../internal/order/postgres_integration_test.go)、
[券生命周期集成测试](../internal/order/coupon_postgres_integration_test.go)、
[退款集成测试](../internal/order/refund_postgres_integration_test.go) 和
[订单仓储实现](../internal/order/repository.go)。

一次测试曾抓到事务语义错误：支付发现订单已过期后，在事务闭包里直接返回业务错误，会回滚刚写入的
关单和库存回补。现在先让事务提交过期副作用，再在事务外返回“订单已过期”。

金额口径也需要讲准：**original_amount - discount_amount = total_amount**；销售 GMV 累计所有曾支付
订单的实付金额（使用 **total_amount**，包含后来退款的订单），退款金额按实际退款日单独统计。
退款成功不抹掉历史支付事实。对应检查与投影在
[对账实现](../internal/reconcile/repository.go)、
[作者销售投影](../internal/attribution/projector.go) 和
[金额约束迁移](../migrations/000019_coupon.up.sql)。

具体决策见架构决策记录中的 ADR-019、ADR-021、ADR-024 和 ADR-029。

### 边界

支付仍是模拟命令，不是支付渠道回调；退款只支持全额，已发货退款不自动回补库存。
优惠券接口当前按 JWT 中的 **user_id** 做商家 owner 范围校验，但没有额外要求 **MERCHANT** 角色；
这不应描述成已完成的角色授权。用券会分别检查持券用户与买家一致、券模板商家与 SKU 商家一致。

## 2. Feed 缓存与游标：缓存可以变旧，分页不能含糊

### 问题

缓存命中率提升后，请求取消、共享对象被修改、缓存失效或排序变化仍可能让请求失败、
读到脏字段，或在翻页时重复和漏项。

### 做法

默认时间流用 **(published_at, id)** 做 keyset 游标，游标带版本并由 HMAC 签名；
下一页从上一页最后一条继续。排序器必须和游标排序键一致，所以关注 Feed 生产路径只使用
支持该游标的时间排序。**HotRanker** 仍是包内排序实验，不能拿它作为该分页路径的生产排序器。

首页缓存只覆盖默认第一页。cache-aside 的 singleflight 加了独立超时上下文，避免第一个请求取消后
把失败广播给所有等待者；缓存结果在填入请求级点赞数前先复制，商品卡在缓存主体之后动态批量装配。
因此缓存不会冻结商品当前上下架状态和售价。

L1 分片 LRU + Redis L2 用版本号跨实例失效：写入删除 L2、删除本机 L1 并递增版本；
其他实例在下一次版本探测时失效本地副本。只有缓存删除和版本递增成功、且版本读取正常时，
探测间隔才约束其他实例发现失效的延迟；删除失败会跳过版本递增，Redis 版本读取失败也可能沿用旧值，
这时只能依赖 TTL 兜底，不能宣称硬上界或强一致。关注 Feed 有单独的第一页缓存；
关注关系变更会失效当前用户缓存，视频发布或删除可能让关注者的缓存最多陈旧一个 TTL。

独立的 **/api/v1/feed/hot** 是 Redis 热榜快照：后台按分钟物化，冻结窗口并用
**(window_end, hot_score, video_id)** 游标稳定翻页；窗口为空或首次读取 Redis 失败时降级到时间流。
这和不支持游标分页的 **HotRanker** 是两条不同路径。

### 证据与取舍

代码与测试分别见 [Feed 服务](../internal/feed/service.go)、
[两级缓存](../internal/feed/tiered_cache.go)、[两级缓存测试](../internal/feed/tiered_cache_test.go)、
[排序器边界测试](../internal/feed/ranker_test.go)、[热榜服务测试](../internal/feed/hot_service_test.go)
和 [热榜缓存测试](../internal/feed/hot_cache_test.go)。性能数字只在
[性能基线](performance-baseline.md) 中按本机、数据集和场景记录；区分历史缓存对照、10月11日完整基线与10月12日逐步优化。Stream缺口已用投递ID追踪定位，安全裁剪/背压和独立消费预算后有限时长交易闭环通过，没有人工重放；短测收敛不等于持续流量容量。

设计取舍见架构决策记录中的 ADR-004、ADR-012、ADR-025 和 ADR-027。

## 3. Outbox 与对账：接受重复投递，用证据恢复投影

### 问题

在订单事务提交后再发事件，进程可能在两步之间崩溃；把外部消息发送塞进数据库事务也不能让
PostgreSQL 与 Redis 原子提交。

### 做法

业务状态与 Outbox 事件在同一 PostgreSQL 事务写入。中继租约领取到期事件并投递 Redis Streams；
投递成功后才更新 Outbox 状态，因此进程在“已投递、未标记”间崩溃时可能重复投递。
语义是**至少一次**，不是恰好一次消息投递。

消费者在一个 PostgreSQL 事务里写 **consumed_events** 回执并更新作者日销投影，提交后才 **XACK**。
所以“没有回执”可以作为“投影事务没有提交”的重放依据；重复事件靠 **event_id** 唯一约束去重。
这只能说明数据库消费事务的幂等效果，不代表消息只投递一次。

对账当前有九项可执行检查：**inventory_balance**、**order_stock_ledger**、
**refund_order_consistency**、**order_payment_consistency**、**author_sales_projection**、
**coupon_issuance**、**coupon_usage**、**order_amount_split**、**outbox_delivery**。
覆盖库存余额与流水、订单及回补流水、退款与支付状态和金额、作者销售投影、券发行与核销、订单金额拆分，
以及已发布事件的消费回执（有宽限期）。对账可按缺失回执重放事件，
也可从订单、订单项和退款事实重建投影。

### 证据与取舍

[Outbox 集成测试](../internal/outbox/integration_test.go) 使用真实 PostgreSQL 与 Redis 验证事务回滚、
重复消息去重、DLQ 重放和未确认消息重新认领；
[对账集成测试](../internal/reconcile/integration_test.go) 覆盖无差异数据、注入差异、
缺失回执重放、读模型重建及券与金额检查。九项检查定义在
[对账仓储](../internal/reconcile/repository.go)。

Redis Streams 是当前已接入的传输；Kafka 没有接入。Outbox 的 **Publisher** 抽象不等于已有 Kafka
实现。Redis 持久化仍有秒级数据损失窗口，因此恢复能力还依赖 PostgreSQL 中的 Outbox、回执、
对账与重建。实现选择见架构决策记录中的 ADR-014、ADR-016 和 ADR-017。

本地演练脚本是辅助观察手段，不能代替上述集成测试：当前 **drill_failure_modes.sh** 的 Redis 阶段
没有实际发起支付，消费崩溃阶段只查看 Stream 状态而没有杀掉消费者，待处理总数还包含未来才到期的
关单事件；RustFS 步骤使用无效对象 key/票证，不能证明熔断行为。**drill_commerce_loop.sh** 上传随机
1 MiB 字节后立即发布，默认媒体门禁开启时不会构成有效视频发布链路。若关闭媒体校验，只能演示
模拟业务链路，不能算媒体验证证据。脚本待修状态见 [工程路线图](roadmap.md)。

## 4. 媒体封存与删除补偿：外部对象存储不参加数据库事务

### 问题

预签名上传若直接写最终对象 key，客户端可重放或替换对象；只看上传凭证也不能证明对象实际存在、
大小符合限制或内容可播放。数据库事务又无法回滚 RustFS 已完成的复制和删除。

### 做法

预签名 PUT 只发到 **staging/** 临时 key，并将声明的 **Content-Length** 与 **Content-Type** 纳入签名。
核销票证时校验用户、对象 key、有效期和一次性状态；服务端读取对象大小并应用硬上限，再用源 ETag
条件复制到最终 key，防止在检查和封存之间替换对象。视频记录与票证核销在 PostgreSQL 事务内写入。

视频发布还有异步媒体门禁：探测状态从 **PENDING** 到 **PASSED**、**REJECTED** 或 **SKIPPED**。
格式损坏或不符合策略时拒绝；ffprobe 工具缺失属于环境问题，标为 **SKIPPED** 并记录告警；
下载或临时错误保留待重试。只有 **PASSED** 或 **SKIPPED** 能发布，服务层检查和数据库约束共同守门。
删除时视频逻辑状态与 **object_delete_jobs** 清理意图同事务提交；Worker 使用 **SKIP LOCKED**
重试物理删除，并清理过期未核销的 staging 对象。

### 证据与边界

[上传封存集成测试](../internal/storage/upload_integration_test.go) 通过真实 RustFS 验证签名长度/类型变更、
硬大小上限和 ETag 替换竞态；[媒体门禁 PostgreSQL 测试](../internal/video/probe_postgres_integration_test.go)
验证数据库发布约束与探测队列；
[探测器测试](../internal/mediaprobe/probe_test.go) 验证 ffprobe 结果与 faststart 处理；
[删除补偿 PostgreSQL 测试](../internal/video/cleanup_postgres_integration_test.go) 验证失败重试、
清理与媒体探测不能复活已删除视频。它们是分层测试，不是一个覆盖 RustFS、PostgreSQL、
ffprobe 的单一端到端演练。

RustFS 复制成功后，如果 PostgreSQL 后续插入或提交失败，数据库无法撤销最终对象；当前清理任务只处理
已知删除任务和过期 staging key，没有扫描发现这类未知最终对象孤儿的回收器。删除任务失败会按间隔
持续重试，目前没有最大尝试次数或独立死信队列。上传门禁、补偿范围及取舍见
架构决策记录中的 ADR-028 和 ADR-029。

## 5. 其他能力与面试快查

| 追问 | 当前实现与可讲的边界 |
|---|---|
| 为什么模块化单体？ | 当前订单事务留在 PostgreSQL 本地事务；模块边界按消费侧接口与独立 read model 划分。见 [后端架构](backend-architecture.md) 和 ADR-001。 |
| 怎么处理身份与可见性？ | JWT access/refresh，refresh 原子轮换与 Redis 撤销；非作者不可见资源统一隐藏为 404，写入条件包含作者和来源状态。见 ADR-008、ADR-009、ADR-011、ADR-020。 |
| 订单怎么防止越权读写？ | 订单接口从 JWT 读取 actor ID，不信任请求体里的 user_id；买家操作按订单买家校验，商家查询/履约按下单时记录的 merchant_id 校验。签名密钥必须显式配置。见 [订单 Handler](../internal/order/handler.go)、[订单生命周期查询](../internal/order/read_lifecycle.go)、[配置加载](../internal/config/config.go)。 |
| 限流为什么曾能被绕过？ | Hertz 默认可信代理过宽会让客户端伪造 X-Forwarded-For 换限流桶；现在默认不信任转发头，只允许显式 TRUSTED_PROXIES，并拒绝信任全网。账号限流 key 与登录账号统一规范化。见 [配置校验](../internal/config/config.go)、[限流测试](../internal/middleware/ratelimit_test.go)。 |
| refresh token 怎么防并发双签发？ | 旧的 check-then-act 允许并发请求同时消费同一 refresh token；现在以 Redis SETNX Claim 原子抢占一次性使用权。服务层并发重放测试使用 fake Claim 验证只签出一组新 token；测试不是 Redis 故障演练。见 [Claim 实现](../internal/user/blacklist.go)、[服务测试](../internal/user/service_test.go)。 |
| 软删除用户还能留在互动列表里吗？ | 点赞/关注和关系列表会过滤 deleted_at 非空或非 ACTIVE 用户；不能依靠外键表达账号可见性。见 [互动仓储](../internal/interaction/repository.go)。 |
| 为什么商品卡不会和缓存 Feed 一起过期？ | 缓存 Feed 主体后再批量读取当前商品关系与商品状态；销售归因使用订单创建时保存的来源快照。见 ADR-003、[商品挂载](../internal/promotion/service.go)、[归因查询](../internal/attribution/repository.go)。 |
| 视频和商品关系有什么边界？ | 只允许作者挂自己的在售商品，最多五个有效关系；数据库 advisory lock 与约束守住并发上限。归因仍不能证明点击来源。见 [商品挂载实现](../internal/promotion/repository.go) 和 [迁移](../migrations/000013_enforce_video_product_active_limit.up.sql)。 |
| 点赞、关注和播放事件怎么幂等？ | 关系表唯一约束配合冲突忽略；播放事件同步写 PostgreSQL，以 event_id 唯一约束及载荷比较拒绝同键异载荷。Kafka 没有接入播放路径。见 [播放事件实现](../internal/video/repository.go)。 |
| 怎么防刷不存在的 ID？ | 进程内双缓冲 Bloom、warmup 保护和 Redis 负缓存；只对确认不存在的记录写负缓存，可见性拒绝不缓存为不存在。见 [存在性防护](../internal/existence) 和 ADR-022。 |
| RustFS 停机时会怎样？ | 对象存储调用通过三态熔断装饰器，业务正常的对象不存在不会计为依赖故障；打开时返回可重试的 503。见 [熔断器](../internal/breaker) 和 ADR-023。 |
| 有完整链路追踪吗？ | 有结构化日志、Prometheus 指标及自研 W3C traceparent 透传；没有 OpenTelemetry SDK/exporter，也没有端到端 span 链路。见 [观测模块](../internal/observability) 与 [traceparent 实现](../internal/tracing)。 |
| 有真实容量结论吗？ | [性能基线](performance-baseline.md) 保存历史缓存对照及新的分步优化证据；100并发热缓存首页合并动态读取的同预算三轮中位吞吐提高43%，动态新鲜度由真实PG测试约束。交易按热点/分散SKU核对回执、库存、账本与九项对账；有限时长结果不宣称生产容量或长期自动收敛。 |
| 怎么避免接口契约和错误语义漂移？ | 错误经过 repository → service → handler 翻译；OpenAPI 有路由同步与 schema/DTO 校验，CI 会运行契约检查。见 [错误响应](../internal/response) 和 [API 契约测试](../internal/api/spec_test.go)。 |

还有一个短而具体的排查案例：一次演练注册返回 201、紧接着登录却返回 401。密码哈希正常，查库发现
新用户的 status 是空字符串；GORM 把结构体零值写进 INSERT，绕过了数据库列默认值。修复在注册时显式写
ACTIVE、模型默认值和数据库约束，并增加真实 PostgreSQL“注册后立刻登录”测试。见
[用户服务](../internal/user/service.go)、[用户模型](../internal/user/model.go) 和
[用户 PostgreSQL 集成测试](../internal/user/postgres_integration_test.go)。

## 6. 必须主动说明的范围

- 消息投递至少一次；不能声称恰好一次投递。Kafka 未接入，播放事件仍同步落 PostgreSQL。
- 项目有可执行集成测试、CI 和运维工具，但不等于完全生产就绪；没有真实支付渠道、多机房灾备、
  生产容量模型或完整 OTel 链路。
- 客户端归因字段只能在当前实现中验证有效关系，不能证明用户真实点击来源；写入路径也未再次验证
  视频当时仍为 PUBLISHED。这是归因边界，不把它描述成可信广告点击归因。
- 优惠券商家 API 当前基于 owner/user_id 授权，未做额外 MERCHANT 角色门禁。
- RustFS 最终复制与 PostgreSQL 提交无法原子化；失败可能遗留未知最终对象，当前没有孤儿扫描回收。
- 故障演练脚本存在待修场景；对账和消息语义以真实依赖集成测试及具体代码为证，不以脚本标题为证。

旧审计、分阶段修复记录和专题笔记中的有效机制与故障经验已压缩到以上四条故事、快查和边界；
具体实现决策统一回指 ADR。历史工作细节应以代码、测试和 ADR 为准。
