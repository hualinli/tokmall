# 架构决策记录（ADR-001～030）

> 更新于 2026-10-09。保留稳定编号供源码与其他文档引用。本文记录仍有解释价值的决策及其当前实现；历史实验、当时的故障故事和旧性能数字不作为现状依据。实现以代码和 [migrations](../migrations/) 为准，系统边界见 [后端架构](backend-architecture.md)。

### ADR-001：JWT 认证，不采用服务端 Session

采用短时 access JWT 与 refresh JWT，便于 HTTP API 无 cookie 状态地认证。代价是 JWT 本身不可撤销，因此当前只对 refresh token 引入 Redis 撤销与轮换（ADR-011）；access token 仍靠短 TTL 失效。签名密钥必须显式配置。实现见 [token manager](../internal/token/token.go) 和 [配置校验](../internal/config/config.go)。

### ADR-002：Feed 路由按场景拆分

全局 Feed、关注 Feed、作者视频列表等场景使用独立路由，便于分别定义认证、限流和 OpenAPI schema。当前路由由 [cmd/api/main.go](../cmd/api/main.go) 注册，契约以 [OpenAPI](../internal/api/openapi.yaml) 为准，route-sync 由 [main_test.go](../cmd/api/main_test.go) 检查。

### ADR-003：商品卡由消费模块按需批量装配

Feed 和视频模块不共享商品模块的 Go model，也不把商品交易事实复制进 Feed。当前按页批量读取并装配 `product_cards`；商品状态在响应装配时读取，见 [Feed 商品卡 read model](../internal/feed/product_cards.go) 和 [视频商品卡 read model](../internal/video/product_cards.go)。早期只传商品 ID 的方案已被当前 API 行为替代。

### ADR-004：关注 Feed 使用拉模式完整时间线

关注 Feed 在请求时读取关注作者的完整候选时间线；低关注量使用有界归并，关注量较大时走全局时间索引。旧的每作者 TOP 5 是历史实现。生产排序只有与 `(published_at, id)` 游标一致的 `TimeRanker`；推模式和 HybridRanker 未实现。实现见 [feed/service.go](../internal/feed/service.go) 和 [ranker.go](../internal/feed/ranker.go)，生产配置入口见 [main.go](../cmd/api/main.go)。

### ADR-005：数据库提交后失效缓存

采用 Cache-Aside，先提交事实写入，再失效派生缓存。当前首页可使用 Redis L2 与进程内 L1；缓存失效失败不会回滚业务写入，短暂陈旧由缓存 TTL 兜底，因此不承诺强一致。实现见 [tiered_cache.go](../internal/feed/tiered_cache.go)。

### ADR-006：Repository、Service、Handler 分层翻译错误

Repository 将 ORM/数据库错误翻译为领域错误；Service 根据业务上下文处理并记录故障；Handler 将领域错误映射为统一 HTTP 错误，未知错误返回 500。错误接口由消费侧定义，避免 service 和 handler 依赖 GORM 类型；统一 HTTP 映射见 [response.go](../internal/response/response.go)。

### ADR-007：数据库 upload ticket 约束上传凭证

上传票证记录用户、对象 key、有效期和一次性核销状态，防止客户端伪造 key 或引用他人对象。当前实现进一步把预签名 PUT 限定到 staging key，并在核销时封存实际字节；初期的“直接上传最终 key、只检查对象存在”已被 ADR-029 的流程取代。见 [video/service.go](../internal/video/service.go)、[video/repository.go](../internal/video/repository.go) 和 [storage.go](../internal/storage/storage.go)。

### ADR-008：视频状态机由应用规则和数据库条件更新共同保护

状态转换集中定义，并在写入时带上来源状态、作者和目标记录条件；用 `RowsAffected` 仲裁并发，而不是依赖先查后写。数据库 CHECK 约束补足不可变状态不变量。媒体探测状态另有发布门禁，见 ADR-028。实现见 [video/service.go](../internal/video/service.go) 与 [迁移 000022](../migrations/000022_probe_publish_gate.up.sql)。

### ADR-009：视频不可见时统一返回 404

已删除视频对所有人不可见；待发布视频只对作者可见。对无权访问的资源统一返回“未找到”，降低枚举资源和推断他人资源归属的风险。服务层策略见 [video/service.go](../internal/video/service.go)。

### ADR-010：测试覆盖分层协议边界

纯业务规则使用单元测试，handler 使用 Hertz 内存链路，数据库约束/事务使用显式启用的 PostgreSQL 集成测试；契约测试检查路由与 OpenAPI、schema 与 DTO，前端另有 Vitest 和 Playwright。集成脚手架使用 [testsupport](../internal/testsupport/testsupport.go) 的隔离 schema。Fake 测试不能替代 SQL、锁和真实错误翻译验证。

### ADR-011：refresh token 黑名单与原子轮换

Redis key 按 refresh `jti` 建立，TTL 为 token 剩余有效期；access token 不查黑名单。轮换使用 `SETNX` 原子 Claim 旧 `jti`，并发刷新只有一次能成功，再签发新 token；登出对无效/过期 refresh token 作为幂等成功处理。Redis 故障会影响 refresh/logout。实现见 [user/service.go](../internal/user/service.go) 和 [blacklist.go](../internal/user/blacklist.go)；前端 token 暂存 localStorage 的权衡见 ADR-018。

### ADR-012：首页默认第一页使用 Cache-Aside 与 singleflight

只缓存默认首页第一页，避免将深页、任意 limit 和复合排序键引入缓存。并发回源用 singleflight 合并，并在合并函数内再次检查缓存。Feed 主体可缓存，商品卡按页动态装配；缓存故障时回源数据库。实现见 [feed/service.go](../internal/feed/service.go)；两级缓存失效见 ADR-025。

### ADR-013：按数据不变量评估数据库约束

唯一约束是幂等写入的基础，CHECK 约束保护状态和金额不变量；外键按写入压力、孤儿容忍度和补偿能力逐表决定，不因“简化”而批量删除。约束变更必须有迁移和明确的应用层/对账兜底，当前结构见 [migrations](../migrations/)。

### ADR-014：Transactional Outbox 默认通过 Redis Streams 传输

业务事实和 Outbox 事件在同一 PostgreSQL 事务写入；Relay 采用租约、退避与死信，Streams 消费组提供确认和待处理消息认领，端到端语义是至少一次。数据库消费回执与投影在同一事务提交，随后才 ACK。Redis Streams 是当前实现；Kafka 仅是 Publisher 抽象的可替换方向，仓库没有已接入的 Kafka broker 或 consumer。实现见 [transport.go](../internal/outbox/transport.go)、[relay.go](../internal/outbox/relay.go) 和 [consumer.go](../internal/outbox/consumer.go)。消费回执和对账重放用于补偿 Redis 的持久性边界。

### ADR-015：退款只支持全额，不自动处理退货入库

退款金额等于订单实付金额；已发货订单退款不回补库存，已完成订单不允许申请。部分退款需要新的退款累计模型；退货入库需要质检和重新上架流程，目前均未实现。见 [refund_service.go](../internal/order/refund_service.go) 和 [refund_repository.go](../internal/order/refund_repository.go)；未发货退款的库存回补和占用券释放见 ADR-029。

### ADR-016：用本地事务、Outbox 和对账替代分布式事务

交易写入在 PostgreSQL 本地事务提交；跨模块副作用通过 Outbox 至少一次投递和幂等消费者完成，周期对账发现缺少回执时可重放。当前不引入 2PC，也没有跨 PostgreSQL、Redis、S3对象存储 的原子事务。见 [outbox](../internal/outbox/) 和 [reconcile](../internal/reconcile/) 实现。

### ADR-017：作者销售投影保留毛额并单列退款

`order_count`、`units_sold` 和 `gmv` 统计曾支付过的所有订单，包括后来退款的订单；退款订单数和金额按退款发生日单列。消费者若需要净成交额再计算毛额减退款。投影见 [attribution/projector.go](../internal/attribution/projector.go)，对账见 [reconcile/repository.go](../internal/reconcile/repository.go)，两边必须使用相同口径。

### ADR-018：当前前端把 token 存在 localStorage，并同步多标签状态

`BroadcastChannel` 与 storage 事件让多个标签页共享轮换后的 token 对和登出状态，避免标签页长期持有已轮换 token。localStorage 中的 refresh token 可被同源 XSS 读取；HttpOnly Cookie、CSRF 防护和前端凭据模式改造尚未实施。实现见 [auth-sync.ts](../web/src/lib/auth-sync.ts) 和 [auth-store.ts](../web/src/lib/auth-store.ts)。

### ADR-019：库存以 PostgreSQL 条件更新为准，不做 Redis 预扣

下单在数据库事务内按库存条件扣减并写库存流水，数据库约束/幂等流水保护不变量。当前没有 Redis 预扣、秒杀队列或异步下单；只有测量到库存行锁是实际瓶颈时才重新评估。扣减和流水见 [order/repository.go](../internal/order/repository.go)。

下单事务在首次查询订单前，对带命名空间的 `(user_id, idempotency_key)` 取 64 位哈希的 `pg_advisory_xact_lock`，在默认 READ COMMITTED 下先等待前次提交，再判断重放/指纹冲突，最后读取商品、锁券和扣库存。这样券已占用或商品已下架不会遮住同键重放。锁随事务结束释放，哈希碰撞仅造成额外串行化；订单唯一约束继续兜底。不能仅锁不存在的订单行，也不使用跨进程无效的内存锁。

### ADR-020：对非作者的资源操作也统一隐藏存在性

非作者对视频执行删除等操作，与资源不存在使用同一 404 语义，避免自增 ID 暴露资源是否存在。权限必须由服务端身份和资源所有权判断，不接受请求体提供的 user ID 替代认证身份。服务层权限策略见 [video/service.go](../internal/video/service.go)，HTTP 身份来自 [auth middleware](../internal/middleware/auth.go)。

### ADR-021：订单超时采用延迟事件加速与轮询兜底

下单事务写入 `commerce.order.timeout.v1`，`available_at` 等于订单到期时间；消费者在到期后尝试条件关单。周期 worker 使用 `FOR UPDATE SKIP LOCKED` 扫描仍待支付订单。支付请求也执行 lazy expiry。三条路径共享数据库订单状态与库存/券释放规则，消息只加速关单，数据库仍是唯一事实源。实现见 [order/repository.go](../internal/order/repository.go)、[timeout_handler.go](../internal/order/timeout_handler.go) 和 [expiry_worker.go](../internal/order/expiry_worker.go)。

### ADR-022：缓存穿透使用进程内布隆过滤器与 Redis 负缓存

布隆过滤器只在“确定不存在”时短路数据库；首次全量加载完成前必须放行请求，重建时新旧缓冲同时查询。Redis 负缓存跨实例复用真实 miss 并带 TTL 抖动。只有数据库确认记录不存在才写负缓存；权限或可见性拒绝不能写，否则会把有权用户的读取错误缓存为 404。见 [existence](../internal/existence/) 和 [video/existence.go](../internal/video/existence.go)。

### ADR-023：对象存储通过三态熔断装饰器隔离故障

熔断器采用 closed/open/half-open 与受限探测；对象不存在和无效大小等业务答复不计为依赖故障，熔断打开映射为可重试的 503。bucket 启动检查失败会记录警告并继续启动。当前 [storage 装饰器](../internal/breaker/storage.go) 将预签名和对象读写都放在同一个熔断器内；预签名通常可基于已缓存的 bucket region 本地签名，但首次 region 未命中可能向 S3对象存储 查询。上传票证仅在预签名成功后写入数据库；因此预签名失败或熔断打开时不会留下新票证，而 region 已缓存时 S3对象存储 不可用也可能仍能签出 URL。具体边界由 S3对象存储 客户端版本及 region 缓存状态决定。

### ADR-024：优惠券 v1 为商家范围满减券

规则集中在 `coupon.DiscountFor`；当前不支持折扣券、叠加券、商品/品类范围。券模板属于一个商家，下单时只允许用于同一商家的 SKU；这是商家范围而非全平台通用券。门槛按整单商品金额计算。领券锁模板行、条件更新发行数并校验用户限领；订单写入原价、优惠和实付金额，数据库检查金额恒等式。见 [coupon/service.go](../internal/coupon/service.go)、[order/repository.go](../internal/order/repository.go) 和 [迁移 000019](../migrations/000019_coupon.up.sql)。退款成功时退回仍有效的券，过期券标记为 `EXPIRED`。

### ADR-025：L1 跨实例失效使用版本号和短周期探测

首页 L1 是进程内分片 LRU，L2 是 Redis。写入后清理本地 L1 和 Redis L2，并递增 `feed:version`；其他实例按配置的探测间隔发现版本变化，L1 TTL 作为最终兜底且不得大于 L2 TTL。Redis 版本读取失败时会沿用旧版本，缓存 fail-open。实现见 [tiered_cache.go](../internal/feed/tiered_cache.go) 和 [配置校验](../internal/config/config.go)。当前实现中若 L2 `Del` 返回错误，失效函数会提前返回、不执行版本 `Bump`；若 `Bump` 失败，远端也不会获知版本变化。这两种情况中业务写仍成功，远端 L1 依赖 TTL 收敛。

### ADR-026：Redis 慢命令日志忽略预期阻塞命令

慢命令 Hook 记录命令名、耗时和 key 前缀，不记录完整的用户/订单 key。`XREADGROUP`、阻塞队列读取及订阅等正常等待命令被排除，避免把消费者等待误判为 Redis 延迟。实现见 [cache/slowlog.go](../internal/cache/slowlog.go)。

### ADR-027：热榜用分钟桶、冻结窗口和分数游标

独立热榜将互动分值写入分钟桶，由后台任务物化为冻结的 v2 窗口；窗口有 2000 条上限和 10 分钟 TTL。分页游标签名包含 `window_end`、`hot_score`、`video_id`，按分数和 ID 严格续页；已存在的窗口不重写，过期窗口拒绝旧游标。首页窗口缺失时 singleflight 合并回源；空首页或首页 Redis 故障会降级为时间流，返回时间游标，后续请求继续时间流。已有热榜游标遇到窗口过期或 Redis 故障会返回错误，不切换排序以避免重复或漏项。实现见 [hot.go](../internal/feed/hot.go)、[hot_cache.go](../internal/feed/hot_cache.go) 和 [hot_service.go](../internal/feed/hot_service.go)。热榜窗口已经具备冻结分数游标，但关注 Feed 的实验 `HotRanker` 仍不支持在线游标分页，不能混为同一实现。

### ADR-028：媒体探测放入异步 worker，并区分跳过与拒绝

上传请求只检查对象存在性/大小；异步 worker 才调用 ffprobe 验证媒体策略，并在需要时用 ffmpeg remux faststart。状态为 `PENDING/PASSED/REJECTED/SKIPPED`：ffprobe 缺失或校验关闭是 `SKIPPED`，文件损坏或不符合策略是 `REJECTED`；只有 `PASSED/SKIPPED` 可以发布。探测结果通过条件更新写回；暂时下载失败只推迟重试，`DELETED` 视频不取件。数据库迁移加入状态约束、重试时间及发布门禁。见 [probe_worker.go](../internal/video/probe_worker.go)、[probe_repository.go](../internal/video/probe_repository.go) 和 [迁移 000020](../migrations/000020_video_media_probe.up.sql)。多个 worker 可能重复探测，当前没有独占租约。

### ADR-029：上传封存、对象清理补偿与订单资源释放

- 预签名 PUT 只写 `staging/`，绑定声明长度和 Content-Type；视频上限 512 MiB、图片上限 5 MiB。服务端 StatObject 校验真实大小后按源 ETag 条件复制到最终 key，避免并发重传替换已校验字节。见 [storage.go](../internal/storage/storage.go) 和 [video/service.go](../internal/video/service.go)。
- 票证核销持有数据库行锁；封存、视频/封面票证核销和视频创建在同一数据库事务中。S3对象存储 封存失败会回滚票证核销和视频创建，仍有效的票证可重试；S3对象存储 复制不是事务性回滚的一部分，复制成功而后续数据库步骤失败时可能留下无引用最终对象。封存期间对象存储调用使用 30 秒 deadline。事务与行锁见 [video/repository.go](../internal/video/repository.go)。
- 删除状态和 `object_delete_jobs` 同事务写入；API 进程内 worker 每 10 秒取任务，删除失败延迟 1 分钟重试。未使用票证和 staging 对象等原 PUT 过期后清理；媒体回写与删除共用视频行锁。旧部署发出的、目标为最终 key 的 PUT URL 不能被新代码撤销，升级时须等待其最长有效期结束。见 [cleanup_worker.go](../internal/video/cleanup_worker.go) 和 [迁移 000023](../migrations/000023_object_cleanup.up.sql)。
- 取消、轮询超时、延迟事件超时、支付时 lazy expiry 和退款成功都释放订单占用券；有效券回到 `UNUSED`，过期券记为 `EXPIRED`，订单继续保留历史引用。唯一索引排除 `REFUNDED/CANCELED/EXPIRED` 订单。实现见 [order/repository.go](../internal/order/repository.go)、[refund_repository.go](../internal/order/refund_repository.go) 和 [迁移 000024](../migrations/000024_closed_order_coupon.up.sql)。
- 退款成功事件和作者日销投影继续使用毛额加单列退款口径（ADR-017）；退款申请本身不改变已支付成交事实。

对应的约束与升级修复位于 migrations 000020～000024：媒体状态/发布门禁、退券后的券重用、对象清理任务和历史数据修复。当前没有累计存储配额、每日上传配额或外部支付渠道结算。

### ADR-030：默认对象存储改用RustFS，应用依赖S3协议

MinIO服务端归档后，开发环境与CI统一使用固定版本RustFS 1.0.1；应用保留minio-go作为S3客户端，不引入RustFS专有管理API。配置改为 `S3_*` / `S3Config`，熔断指标标识为 `s3`，服务端部署参数使用 `RUSTFS_*`。业务模块继续通过消费侧接口调用上传签名、对象检查、ETag封存、读写和删除，不修改订单、库存或视频表结构。

新部署使用独立的 `rustfs-data` 卷，不把旧MinIO卷直接交给RustFS，不包含历史对象搬迁；已有 `MINIO_*` 环境变量须显式改名。浏览器直传和Range播放仍走预签名S3 URL。签名长度/类型、源ETag条件复制、CORS、URL到期、删除幂等性由真实存储契约测试约束；CI backend与E2E都运行RustFS。历史压测源码包、环境和数据保留原样，旧MinIO上的吞吐不自动成为RustFS容量结论。参见 [storage](../internal/storage/storage.go)、[契约测试](../internal/storage/upload_integration_test.go)、[Compose](../docker-compose.yml) 和 [运维](development-and-operations.md)。
