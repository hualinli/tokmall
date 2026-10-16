# 关键设计取舍

> 核对日期：2026-10-13。本文按当前源码归并为八个主题，只记录选择原因、承担的代价及重新评估条件。具体规则、操作和实验分别维护在 [商城业务](commerce-business.md)、[后端架构](backend-architecture.md)、[商城技术实现](commerce-v1-technical-design.md)、[开发与运维](development-and-operations.md) 和 [性能证据](performance-baseline.md)。文件路径保留，旧编号历史从 Git 查看。

<a id="monolith"></a>
## 模块化单体与同库事务

**选择与原因：** 模块按业务组织，共享 PostgreSQL，由 [main.go](../cmd/api/main.go) 装配消费侧接口；跨模块读取使用各自 read model / SQL，持久化 model 不作为跨模块 API。订单、库存、券和 Outbox 在同库事务内提交，让交易不变量由事务及数据库约束守住，适合当前开发和部署规模。

**代价：** 模块仍共享数据库与进程故障域；order/product/inventory 都有库存写入，边界并非独立服务级隔离。PostgreSQL 与 Redis/RustFS 之间没有原子事务，需要缓存失效、事件幂等和对象清理补偿。

**重新评估条件：** 出现独立部署、团队边界或明确的容量隔离需求，并有测量证明收益时，再设计跨服务一致性。模块职责见 [后端架构](backend-architecture.md#运行时与依赖边界)。

<a id="events"></a>
## Outbox 与 Redis Streams

**选择与原因：** 事实与 Outbox 同事务，Relay 通过 Redis Streams 传输。复用现有 Redis，获得消费组、确认和 PEL 恢复能力；消费回执与业务投影同事务，提交后才 XACK，端到端按至少一次处理。实现见 [中继](../internal/outbox/relay.go)、[传输](../internal/outbox/transport.go) 和 [消费者](../internal/outbox/consumer.go)。

**代价：** Redis 的持久化故障仍可能丢失已发布消息，依靠 PG 事件、回执和对账重放恢复。发布按全部消费组确认水位回收；容量满时背压，未追加事件留在 PG，不计普通投递失败。遗留消费组会阻止回收，有限缓冲不能保证长期积压收敛。PG 内 DEAD 与消费侧 DLQ 是两个恢复位置。

**重新评估条件：** 持续生产速率超过消费速率，或需要独立消息基础设施、分区和保留能力时，先测消费、连接池与入口保护，再评估 broker。当前 Kafka 未接入，替换 Publisher 之外仍需实现消费者与恢复流程。操作见 [观测与恢复](development-and-operations.md#观测与恢复)。

<a id="inventory"></a>
## PostgreSQL 库存与热点锁

**选择与原因：** 数据库条件扣减、库存非负 CHECK 和业务键唯一流水共同守住库存；下单先准备订单、明细、流水、券和超时事件，最后更新热点库存并提交，缩短成功路径持锁时间。同键请求用事务级 advisory lock 串行化，再判定重放和指纹冲突。实现见 [订单仓储](../internal/order/repository.go) 与 [创建与幂等](commerce-v1-technical-design.md#创建与幂等)。

**代价：** 热点 SKU 仍串行争锁；售罄请求会做最终回滚的准备写入。消费者有独立连接预算，保护连接获取机会，无法隔离数据库 CPU、磁盘或业务行锁。

**重新评估条件：** 同条件压测确认热点锁成为瓶颈，且入口保护和缩短事务仍不足时，再评估排队或 Redis 预扣及其补偿成本。当前没有异步下单或秒杀队列。证据见 [性能证据](performance-baseline.md#latest-results)。

<a id="timeline"></a>
## 完整关注时间线与独立热榜

**选择与原因：** 关注流请求时拉取完整时间线，生产仅使用与 `(published_at,id)` 游标一致的 TimeRanker，避免每作者固定 TOP N 漏项。热榜独立使用分钟桶和冻结窗口，签名游标携带窗口、分数、ID，让窗口内翻页稳定。实现见 [时间流](../internal/feed/service.go)、[排序](../internal/feed/ranker.go) 与 [热榜](../internal/feed/hot_service.go)。

**代价：** 拉模式查询成本随关注量变化；热榜有条数上限和过期时间。首页可降级为时间流并沿用时间游标；已有热榜游标不能中途换排序，窗口到期或 Redis 故障时返回错误。关注流实验 HotRanker 不支持在线游标分页，配置与 service 均拒绝使用。

**重新评估条件：** 关注规模和读写比例的测量证明拉模式受限时，再设计推拉混合、补历史与收件箱游标协议。当前没有个性化推荐或关注推送扇出。细节见 [Feed、排序与缓存](backend-architecture.md#feed排序与缓存)。

<a id="cache"></a>
## 有限缓存与请求时动态装配

**选择与原因：** 只缓存默认大小首页的第一页视频主体，采用 Cache-Aside、singleflight 和回源前二次检查；可选进程 L1 + Redis L2 用版本号探测跨实例失效。点赞数和商品卡按页合并查询，先分别聚合再连接，保持动态事实新鲜且避免 N+1 和计数放大。实现见 [缓存](../internal/feed/tiered_cache.go)、[动态读取](../internal/feed/dynamic_read.go) 和 [装配](../cmd/api/main.go)。

**代价：** 缓存最终一致；业务提交成功后失效失败不会撤销业务事实。实现先删本地 L1、再删 L2，L2 删除失败会跳过版本递增；版本递增或读取失败也会扩大远端陈旧窗口。L1 到期后仍可能读到尚未到期的旧 L2，不能把探测间隔或 L1 TTL 当作所有故障下的总上界。在途请求还可能在失效之后回填旧查询结果，版本号没有把查库与缓存回填变成原子操作。关注缓存主要依赖用户失效和 TTL，发布/删除不遍历粉丝 key。缓存命中仍访问动态字段数据库。

**重新评估条件：** 明确的新鲜度要求或测量证明动态查询成本不可接受时，再设计版本协议、读时过滤或独立缓存。Bloom 目前仅作本地集合观测，阴性不代表全局不存在，只有真实 DB miss 的共享负缓存可短路；实现与测试见 [存在性边界](backend-architecture.md#存在性判定)。

<a id="media"></a>
## staging 封存、异步探测与持久化清理

**选择与原因：** 预签名 PUT 仅写 staging，绑定大小和类型；服务端检查真实大小并按源 ETag 条件复制最终对象。票证核销与创建持数据库锁并同事务；媒体探测放异步 worker，发布以 PASSED/SKIPPED 为门禁。删除状态与清理任务同事务，存储失败由 worker 重试。实现见 [上传服务](../internal/video/service.go)、[存储](../internal/storage/storage.go)、[探测](../internal/video/probe_worker.go) 与 [清理](../internal/video/cleanup_worker.go)。

**代价：** 锁内存储调用会占用数据库连接；复制成功但 PG 回滚可留下最终孤儿。多 worker 可重复探测，条件写回只接受一次结果；工具缺失/关闭探测记 SKIPPED，不能视为已验证安全。staging 清理需等 PUT 到期，物理删除前旧 GET URL 可能仍有效。默认 RustFS，应用依赖 S3 协议，旧对象迁移需独立处理。

**重新评估条件：** 存储成本、慢依赖或公开部署需求明确时，再设计累计配额、孤儿回收、封存租约；真实媒体和对象存储契约测试用于验证该边界，运行与升级见 [运维指南](development-and-operations.md#媒体与旧版本升级)。

<a id="auth"></a>
## JWT 轮换与浏览器凭据

**选择与原因：** access JWT 短时有效，refresh 按 jti 使用 Redis SETNX 原子抢占，一次旧 token 只能成功签发一组新凭据。浏览器持久化到 localStorage，并通过 BroadcastChannel/storage 事件同步登录态。实现见 [refresh](../internal/user/service.go)、[原子 Claim](../internal/user/blacklist.go)、[前端持久化](../web/src/lib/auth-store.ts) 和 [标签同步](../web/src/lib/auth-sync.ts)。

**代价：** access 不逐请求检查撤销或账户状态；refresh/logout 依赖 Redis，Claim 成功后签发失败需重新登录。登录态广播不能仲裁同时刷新多个标签页，竞争失败仍可能发生；localStorage 凭据可被同源 XSS 读取，HttpOnly Cookie/CSRF 改造未实现。资源权限仍须服务端校验归属，不能从 JWT 或前端按钮推断商家角色门禁。

**重新评估条件：** 要求账户即时停用、会话管理或更强浏览器凭据保护时，定义撤销与 Cookie/CSRF 协议。当前认证、限流与可见性规则见 [后端架构](backend-architecture.md#认证与可见性) 和 [前端说明](../web/README.md)。

<a id="recovery"></a>
## 对账恢复与维护窗口重建

**选择与原因：** 九项对账核对交易、库存、券、事件与投影；重放沿用 event_id 和实际消费组回执。日销投影重建要求该组支付/退款事件回执齐全，并在同事务取得 NOWAIT 表锁后检查和改写，避免快照已含事实又被旧事件累加。实现见 [重建](../internal/reconcile/repository.go)、[门禁测试](../internal/reconcile/rebuild_integration_test.go) 与 [对账服务](../internal/reconcile/service.go)。

**代价：** 重建需要暂停相关写入的维护窗口，争锁或缺回执则拒绝；不能删回执或伪造回执绕过门禁。对账不自动修复订单、支付、库存、券等业务事实。PEL 分页 XPENDING/XCLAIM 保留扫描位置，避免只扫头部；恢复耗时随页数和周期变化，没有固定恢复 SLA。

**重新评估条件：** 数据规模或可用性要求不允许停写时，再设计快照与增量切换，保留事实和回执完整性证明。毛额与退款口径见 [商城技术实现](commerce-v1-technical-design.md#推广归因与对账恢复)，维护步骤见 [观测与恢复](development-and-operations.md#观测与恢复)。
