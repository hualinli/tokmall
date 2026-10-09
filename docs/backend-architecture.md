# 后端架构与模块边界

> 更新于 2026-10-09。本文描述当前工作区实现；运行时装配以 [cmd/api/main.go](../cmd/api/main.go)、配置以 [config.go](../internal/config/config.go)、接口以 [OpenAPI 契约](../internal/api/openapi.yaml)、数据库结构以 [版本化迁移](../migrations/) 为准。TokMall 是共享 PostgreSQL 的模块化单体，尚未拆分服务。

## 运行时与依赖边界

`cmd/api` 是组合根：加载并校验配置，连接 PostgreSQL 和 Redis，执行 SQL 迁移，创建 RustFS 客户端、缓存、指标、各模块服务与 handler，再统一注册路由。API 进程也运行对象删除、媒体探测、热榜物化、缓存存在性重建、订单到期、Outbox 中继/消费和周期对账等后台任务；其中多项由配置开关控制。退出时先关 HTTP，再按逆序停止 worker。

请求经过 Hertz 中间件（恢复、request ID、traceparent、HTTP 指标和访问日志）后进入 handler，相关实现见 [tracing middleware](../internal/middleware/tracing.go)、[auth middleware](../internal/middleware/auth.go) 和 [HTTP metrics](../internal/observability/http.go)。`registerRoutes` 将 optional-auth 与 protected 路由分组，并在 API 组挂载读限流、在受保护组挂载写限流。handler 解析 HTTP DTO、注入认证身份并映射业务错误；service 执行业务规则；repository 负责 SQL、事务和 ORM 错误翻译。service 暴露领域参数/视图，不把 GORM model 作为跨层 API。接口通常定义在消费侧，装配关系集中在 `main.go`。

| 层或依赖 | 当前职责与边界 |
|---|---|
| PostgreSQL | 交易事实、状态机、幂等约束、Outbox、消费回执、统计投影和删除任务；订单与库存可在同一数据库事务中协调。schema 只由 golang-migrate 迁移管理，禁止 AutoMigrate。 |
| Redis | Feed 缓存与版本号、关注 Feed 缓存、refresh token 撤销、固定窗口限流、穿透负缓存、热榜分钟桶/窗口，以及 Redis Streams 事件传输。它不替代 PostgreSQL 的交易事实。 |
| RustFS | 私有媒体对象；上传通过绑定长度和类型的预签名 PUT 写 staging key，服务端封存到最终 key；播放/封面 URL 由服务端签发。删除失败由数据库任务补偿。 |
| 模块边界 | 不把模块的持久化 model 当作跨模块 API；跨模块读取使用本模块 read model 或显式 SQL。存在少量窄域协作，例如订单调用 `coupon.DiscountFor`、通过 Outbox API 写事件；需要依赖抽象时优先由消费侧定义接口并由 `main.go` 装配。 |

### 模块职责与事实归属

| 模块 | 职责 | 主要事实或读模型 |
|---|---|---|
| `user` | 注册、登录、refresh 轮换、登出、本人资料 | `users`；Redis refresh 黑名单 |
| `video` | 上传票证、封存、视频状态与可见性、播放事件、作者视频列表、媒体探测与对象清理 | `videos`、`upload_tickets`、`play_events`、`object_delete_jobs` |
| `interaction` | 点赞/关注幂等写入、社交列表 | `likes`、`follows` |
| `feed` | 全局和关注 Feed、签名游标、缓存、商品卡装配、独立热榜窗口 | Feed 为读模型；不拥有视频/交易事实 |
| `product` / `inventory` | 商品与 SKU、上/下架；库存调整和流水 | `products`、`skus`、`inventory`、`inventory_transactions`；订单事务也会扣减或回补库存 |
| `promotion` | 发布视频挂载、排序和取消商品关系 | `video_products` |
| `order` | 单 SKU 下单、模拟支付、取消/超时、买卖双方查询与履约、全额退款 | `orders`、`order_items`、`payments`、`refunds` |
| `coupon` | 商家券模板、领券、持券查询和满减规则 | `coupons`、`user_coupons` |
| `outbox` | 事务 Outbox 中继、Streams 消费、回执、DLQ 与重放 | `outbox_events`、`consumed_events` |
| `attribution` | 作者成交实时查询、事件驱动的日销投影 | `author_sales_daily`；来源订单仍由 `order` 持有 |
| `reconcile` | 交易/库存/投影/券/事件差异检测、事件重放与投影重建 | 跨模块只读校验与修复入口 |
| `breaker` / `existence` / `tracing` | RustFS 熔断装饰器、布隆过滤器与负缓存、W3C traceparent 解析生成 | 横切能力，不拥有业务表 |

订单、库存和 outbox 共享 PostgreSQL 事务是当前模块化单体的一致性边界，不等同于跨服务事务。Feed、视频详情读取商品卡时按页批量装配；订单创建重新校验 SKU、商家和在售状态，并验证客户端提供的来源与活动商品关系后保存商品/价格快照；挂载时要求视频已发布，但下单只校验活动关系，不复查视频当前状态，也不证明用户真实点击来源。推广规则当前要求视频作者就是商品商家；订单会分别保存商家和来源作者。

## 关键在线流程

### Feed、排序与缓存

全局 Feed 使用 `(published_at, id)` 时间游标；游标带 HMAC 签名和排序版本，表示上一页末项。关注 Feed 采用拉模式，读取关注作者完整时间线：低关注量走有界归并，关注量较大时使用全局时间索引；生产排序只有 `TimeRanker`，配置或 service 注入 `HotRanker` 会被拒绝。见 [feed/service.go](../internal/feed/service.go)、[cursor.go](../internal/feed/cursor.go) 和 [ranker.go](../internal/feed/ranker.go)。每作者 TOP 5 是历史实现，不是当前语义；推模式和 `HybridRanker` 尚未实现。

首页默认第一页使用 Cache-Aside、singleflight 和回源前二次检查。可选 L1 分片 LRU 位于 Redis L2 前；L1 TTL 不得长于 L2 TTL。写路径在数据库提交后尝试删除 L2、本地 L1 并递增 Redis `feed:version`，其他实例按版本探测间隔发现失效；当前若 L2 删除失败，版本递增会被跳过；若版本递增本身失败，远端实例也不会及时获知变化。失效调用方记录错误但不回滚业务写，这两种错误路径都依赖远端 L1 TTL 收敛。版本读取失败也会沿用上次值，因此缓存是最终一致而非强一致。商品卡在页响应时动态批量读取，不固定在 Feed 主体缓存内。

`GET /api/v1/feed/hot` 是单独的物化热榜，不是关注 Feed 的 `HotRanker`。互动先更新 Redis 分钟桶，后台每分钟物化冻结窗口；窗口 key 为 v2，条目上限 2000、TTL 10 分钟。游标同时包含 `window_end`、`hot_score` 和 `video_id`，窗口不重写，过期窗口对应游标被拒绝。窗口缺失时才 singleflight 回源；空首页窗口或首页请求遇到 Redis 故障时降级时间流，返回时间游标并在后续请求中继续时间流。已有热榜游标遇到 Redis 故障返回错误，避免切换排序后重复或漏项。实现见 [hot_cache.go](../internal/feed/hot_cache.go) 和 [hot_service.go](../internal/feed/hot_service.go)。

### 上传、发布与清理

上传 URL 只允许写入 `staging/<object_key>`，签名绑定声明的 Content-Length 和 Content-Type。视频请求范围为 1 MiB–512 MiB，图片为 1 KiB–5 MiB；服务端在封存时再次检查对象真实大小，并用源 ETag 条件复制到最终 key。服务端只有在预签名成功后才持久化票证；同一个 RustFS 熔断器也包住预签名。SDK 可在 bucket region 已缓存时本地签名，首次 region 未命中则可能查询 RustFS，因此 RustFS 故障时既可能无法签发票证，也可能在 region 已缓存时拿到 URL。核销时锁住票证行，封存、视频和封面票证核销及视频创建由同一数据库事务控制；封存失败会回滚票证核销与视频创建，票证在有效期内仍可重试。对象存储副作用不能回滚，复制成功但事务失败时可能留下无引用的最终对象。流程见 [video/service.go](../internal/video/service.go)、[video/repository.go](../internal/video/repository.go)、[storage 装饰器](../internal/breaker/storage.go) 和 [storage.go](../internal/storage/storage.go)。

新视频从 `PENDING_REVIEW` 开始。媒体 worker 下载对象到临时目录后调用 ffprobe 检查时长、尺寸和编码，必要时用 ffmpeg remux 以启用 faststart。结果为 `PENDING/PASSED/REJECTED/SKIPPED`：ffprobe 未安装或探测被配置关闭是 `SKIPPED`，损坏/不符合策略为 `REJECTED`；只有 `PASSED` 或 `SKIPPED` 可发布。下载等暂时失败保留待处理并推迟重试，删除视频不被 worker 取件。数据库条件写回保证结果幂等，但多个 worker 仍可能重复做探测。实现见 [probe_worker.go](../internal/video/probe_worker.go)、[probe_repository.go](../internal/video/probe_repository.go)、[migration 000020](../migrations/000020_video_media_probe.up.sql) 和 [migration 000022](../migrations/000022_probe_publish_gate.up.sql)。

删除先在事务内写入对象清理任务，再尝试同步删除；失败任务由 API 进程内 worker 重试。staging 对象要等原 PUT 有效期结束后才可安全清理，过期未用票证也会清理。对象被物理删除前，已签发的 GET URL 可能仍可访问。补偿实现见 [cleanup_worker.go](../internal/video/cleanup_worker.go) 和 [migration 000023](../migrations/000023_object_cleanup.up.sql)。

### 订单、退款与优惠券

下单限单 SKU。服务端读取仍在售的商品/SKU；如请求带来源，会验证客户端提供的来源与 `video_products` 活动关系，但不复查视频当前状态，也不证明用户真实点击来源。推广挂载时要求视频已发布。下单随后锁定并扣减库存，在同一事务写订单项快照、库存流水和 `available_at = expires_at` 的 Outbox 超时事件。支付接口是学习环境的模拟支付请求，不是支付渠道回调。过期订单由延迟事件加速关闭，轮询 worker 是安全网；支付时也做 lazy expiry，数据库状态是唯一事实源。履约转换使用独立状态转换规则，不能借退款状态图绕过退款裁决。

退款仅支持全额；完成态不允许申请。订单进入 `REFUND_REQUESTED` 后等待商家裁决，主订单状态与退款单状态分别约束。退款成功时，原状态为 `PAID`（未发货）则回补库存，原状态为 `FULFILLING`（已发货）不回补；被拒绝则恢复申请前状态。取消、三条超时关闭路径（延迟事件、轮询、支付 lazy expiry）和退款成功都会释放占用券；未过期券回到 `UNUSED`，过期券进入 `EXPIRED`，订单保留历史券引用。

券模板归属商家，当前是商家范围满减券，不支持商品/品类范围、折扣券或叠加。下单只接受与 SKU 商家相同的券；门槛按整单商品金额计算（当前单 SKU 即该行金额）。领取通过锁券模板、条件更新发行数和限领校验防超发；下单锁持券后使用唯一的 `coupon.DiscountFor` 规则。订单金额存原价、优惠、实付，并由数据库约束 `total = original - discount`。实现见 [coupon/service.go](../internal/coupon/service.go)、[order/repository.go](../internal/order/repository.go) 和 [migration 000019](../migrations/000019_coupon.up.sql)。退款申请不改变成交投影；成功退款按发生日单独统计。

## 事件与对账

业务事件与交易事实在同一 PostgreSQL 事务写入 Outbox。Relay 领取到期事件、租约发送并退避重试，最终可进入 DLQ；默认传输为 Redis Streams 消费组，利用 XACK 和 XAUTOCLAIM 实现至少一次投递。消费者在一个数据库事务中写消费回执和投影，提交后确认 Streams 消息；相同 event ID 可幂等重放。见 [outbox/relay.go](../internal/outbox/relay.go)、[consumer.go](../internal/outbox/consumer.go) 和 [attribution/projector.go](../internal/attribution/projector.go)。Kafka 只是 Publisher 可替换方向，当前没有 Kafka broker 或 Kafka 消费者接入。

当前对账有九项可执行 SQL 检查：`inventory_balance`、`order_stock_ledger`、`refund_order_consistency`、`order_payment_consistency`、`author_sales_projection`、`coupon_issuance`、`coupon_usage`、`order_amount_split`、`outbox_delivery`。毛额统计覆盖曾支付过的全部订单（含后来退款订单），退款按发生日另计。实现见 [reconcile/repository.go](../internal/reconcile/repository.go) 和 [cmd/reconcile/main.go](../cmd/reconcile/main.go)。Redis Streams 丢失窗口由消费回执和数据库对账补偿，不能宣称 broker 本身零丢失。

## 配置与可观测性

`JWT_SECRET` 和 `FEED_CURSOR_SECRET` 在所有环境必填；生产环境进一步校验密钥长度/区分、数据库和 Redis TLS、RustFS TLS、占位凭证等。`TRUSTED_PROXIES` 默认空，表示不信任转发头；不允许配置全网段。限流使用 Redis 固定窗口 Lua：认证按账号及 IP，API 读请求按 IP，受保护写请求按用户；Redis 故障时 fail-open 并记录告警日志，见 [config.go](../internal/config/config.go) 和 [ratelimit.go](../internal/middleware/ratelimit.go)。Prometheus `/metrics` 默认绑定 `127.0.0.1:9091`，启动后会主动探测端点。

日志由 slog 输出 stdout 和轮转文件；ContextHandler 从 context 注入 request_id、trace_id、span_id、路由模板和用户 ID，并按字段名脱敏。HTTP 指标使用路由模板，避免把资源 ID 作为标签。实现见 [logcontext.go](../internal/observability/logcontext.go)、[traceparent.go](../internal/tracing/traceparent.go) 和 [HTTP metrics](../internal/observability/http.go)。这里只实现 W3C traceparent 的校验、生成与透传，没有完整 OpenTelemetry SDK、OTLP exporter 或跨服务 span 自动埋点。Prometheus 规则在 [tokmall.yml](../observability/prometheus/rules/tokmall.yml)，告警规则文件不等同于部署了 Alertmanager。

## 明确边界

- 当前是单进程模块化单体；媒体 worker、过期关单、Outbox 和对账不是独立部署的服务。
- Kafka、支付渠道、部分退款/退货入库、券商品范围、Feed 推送扇出及 HybridRanker 均未接入。
- PostgreSQL 与 Redis/RustFS 之间没有分布式事务；通过 Outbox、幂等、补偿任务和对账处理已知失败窗口。

进一步的订单字段、状态约束和迁移依据见 [Commerce v1 技术实现](commerce-v1-technical-design.md)；设计取舍见 [ADR](architecture-decisions.md)，后续演进计划见 [roadmap](roadmap.md)。
