# 架构决策记录（ADR-001～013）

> 从早期 Feed 补充手册提取的历史决策，保留当时的取舍，不作为当前功能清单。当前行为以代码、迁移、OpenAPI 和 [`README.md`](README.md) 为准。2026-09-26 整理。

## 当前适用性提示

| ADR | 今天应如何理解 |
|---|---|
| 001、011 | JWT 仍在用；Redis 已用于 refresh 黑名单和轮换，001 中“未引入 Redis”的背景只属于 V1。 |
| 002 | 当前路由按场景区分。 |
| 003 | 早期只返回商品 ID 的选择已被批量商品卡装配替代；以现有 Feed/视频接口为准。 |
| 004 | 拉模式仍在用；每作者 K=5 已被完整时间线查询替代。原文仅是历史决策，见 [`feed-evolution-plan.md`](feed-evolution-plan.md) F0。文中的演进阈值也是旧假设。 |
| 005 | 先写库后删缓存仍是基础策略，但并发旧值回填和个性化缓存失效需要单独验证；不能把本 ADR 当作严格一致性的证明。 |
| 006～009、013 | 设计原则仍有参考价值；具体表、约束和状态以迁移及代码为准。 |
| 010 | “E2E 无法进 CI”是当时限制，后续 CI 能力需重新评估。 |
| 012 | 首页面缓存仍在用；商品卡和点赞数在请求时动态装配。性能数字是当次实验，不代表当前容量。 |

## 历史决策原文

### ADR-001：选择 JWT 而非 Session（V1）

**上下文**：Feed v1 需要认证机制识别当前用户。

**候选方案**：
1. JWT Bearer Token（无状态）
2. Session + Redis Cookie（有状态）

**决策**：V1 选择 JWT。

**理由**：
- 减少架构复杂度：Redis 的引入应与缓存需求绑定，而非认证需求
- 接口测试友好：无需 cookie 管理
- Payload 可直接携带 user_id/role，避免 token 解码后额外查询

**代价**：
- Token 签发后无法主动失效（除非建黑名单）
- Payload 大小固定，不适合携带大量数据
- 密钥轮换需要优雅过渡

**V2 演进条件**：
- 需要实时封禁用户
- 需要检测 token 在多设备间被盗用
- 认证复杂度已证明值得引入 Redis 会话存储

### ADR-002：Feed 路由按场景分路径

**上下文**：Feed 有不同的获取场景——全局时间线、关注 Feed、作者主页。

**候选方案**：
1. 统一端点 + `type` 参数：`GET /api/v1/feed?type=global`
2. 按场景分路径：`GET /api/v1/feed`、`GET /api/v1/feed/following`、`GET /api/v1/users/{id}/videos`

**决策**：方案 2（分路径）。

**理由**：
- 路由层可直接对 `/feed` 和 `/feed/following` 配置不同的限流策略
- 中间件可差异化：`/feed/following` 必然要求认证，`/feed` 可匿名（可选）
- OpenAPI 文档更清晰：每个场景有独立的 path 和 schema

**代价**：
- URL 比方案 1 多出几个端点
- 但如果后续增加"热门 Feed"（`/feed/trending`）、"推荐 Feed"（`/feed/recommended`），扩展性优势更明显

### ADR-003：Feed 商品信息使用松耦合设计

**上下文**：Feed v1 为 Commerce 模块预留接口。Feed 列表中是否应该嵌入完整的商品信息。

**候选方案**：
1. 紧耦合：Feed item 直接嵌套 product 对象
2. 松耦合：Feed item 只返回 `product_ids` 数组

**决策**：方案 2（松耦合）。

**理由**：
- 当时 Feed v1 尚未引入 Commerce，松耦合有利于先独立开发和测试
- 商品详情有自己的缓存策略和错误处理，不应让 Feed 的响应延迟受商品查询影响
- 当前已按页批量装配 `product_cards`；本段仅保留早期方案比较

**代价**：
- 客户端需要额外一次请求获取商品详情
- 当前由 Feed/视频模块直接装配商品卡，未引入 API Gateway 或 BFF

### ADR-004：拉模式 + K=5 作为关注 Feed 默认策略

**上下文**：关注 Feed 需要展示用户关注作者的最新视频。

**候选方案**：
1. 推模式：发布时写入粉丝收件箱
2. 拉模式 + K=5：请求时实时查询

**决策**：V1 选择拉模式 + K=5。

**理由**：
- V1 用户规模小，拉模式足够支撑
- 推模式引入 Kafka fanout worker，复杂度高
- K=5 × 最多 1000 关注 = 最多 5000 条候选，内存归并可接受
- 关注列表缓存命中率高，进一步降低延迟

**代价**：
- 用户每次刷 Feed 都需要做数据库查询
- 关注数 > 1000 的用户 feed 加载较慢（可通过截断缓解）

**V2 演进条件**：
- 关注数超 1000 的用户占比 > 10%
- Feed 读取 P99 > 500ms
- 此时启动推拉混合实验

### ADR-005：先写数据库再删缓存

**上下文**：视频发布、删除、状态变更后，需要保证缓存最终与数据库一致。

**决策**：采用 Cache-Aside + 先写数据库再删缓存。

**理由**：
- 写数据库成功 → 删缓存 → 下次读重建，数据最终一致
- 如果使用先删缓存再写数据库，在并发读下会导致脏数据写入缓存
- 延迟双删（先删→写→再删）可进一步降低不一致窗口，但 V1 不引入——面试时能讨论即可

**代价**：
- 删缓存失败会导致短暂不一致，但 TTL 到期后自动修复
- 极端场景（删缓存失败且 TTL 很长）可通过消息队列补偿

### ADR-006：错误三层处理链

**上下文**：user 模块首个 repository 直接向调用方抛 `gorm.ErrRecordNotFound` 等 ORM 错误，service/handler 被迫 import gorm 才能判断错误类型。

**候选方案**：
1. 全链路共用 ORM 原始错误
2. 每层各自定义错误、层层翻译
3. repo 翻译成领域哨兵 → service 判定分类 → handler 映射 HTTP（选定）

**决策**：方案 3。

**理由**：
- 错误词汇表只有一份（领域哨兵），由 repo 的实现方从 ORM 错误翻译；接口属于消费者，哨兵与接口同包定义
- service 是唯一有业务上下文的地方，负责"已知/未知判定 + 日志分级 + 上下文包装"：已知业务失败（邮箱重复）不打 Error 日志，未知故障（死锁）才 slog.Error + `%w` 包装
- handler 无领域知识，只做哨兵 → BizError 的映射，未知错误兜底 500
- 判断规则：repo 只包装"service 需要 errors.Is 分支"的错误，其余原样上抛

**代价**：
- 每层多一层映射代码
- 错误类型数量必须克制：错误数量 ≈ 业务分支数量，否则退化成分类学

**V2 演进条件**：
- 微服务化后需引入 RPC 错误码协议（如 gRPC status），哨兵映射到协议错误码

### ADR-007：upload_tickets 防伪造上传凭证（V1）

**上下文**：创建视频时必须校验 object_key 是 upload-url 阶段发放的，防止客户端伪造 key 引用他人文件。

**候选方案**：
1. 客户端幂等 key + 内存/Redis 缓存已发放 URL
2. `upload_tickets` 表：upload-url 写入凭证，创建视频时条件核销（选定）
3. 仅校验 object_key 格式（videos/{user_id}/{uuid}.mp4）

**决策**：方案 2。

**理由**：
- 凭证必须可被多实例共享查询/核销，内存方案在多实例下失效；Redis 是 M4 的引入项，不为凭证提前引入
- 条件核销（`WHERE user_id=? AND object_key=? AND used=false AND expires_at>now()`）由数据库保证"一次性"，无需事务锁
- 校验顺序：StatObject（文件真实存在）→ ConsumeTicket（核销）→ CreateVideo（落库）。StatObject 失败不烧凭证，客户端可重试
- NoSuchKey 由 storage 层翻译为 `ErrObjectNotFound`（哨兵属于产生它的包），service 映射为 400 UPLOAD_URL_EXPIRED 而非 500

**代价**：
- 表会积累过期未用凭证，需定时清理（V1 未做，量级可忽略）
- 没有 Idempotency-Key：客户端重试会拿到新 URL，但不会产生脏数据

**V2 演进条件**：
- 引入 Redis 后可用 Idempotency-Key 语义（同键 5 分钟内返回同一 URL）
- 凭证清理任务随孤儿对象清理一起实现

### ADR-008：视频状态机的双层防护

**上下文**：视频状态转换（PENDING_REVIEW → PUBLISHED/DELETED 等）必须防并发竞态和非法迁移。

**候选方案**：
1. 应用层先查后改（Read-Modify-Write），无数据库约束
2. 表驱动状态机 + DB 条件更新（`WHERE id=? AND status=from`）（选定）

**决策**：方案 2。

**理由**：
- 状态机表集中定义合法迁移（`canTransition`），替换 if-else 瀑布，未来插入真实审核流程只改表不改结构
- DB 条件更新 + RowsAffected 判定：并发请求只有一个能命中前置状态，其余返回 ErrStatusConflict，竞态被数据库强制而非应用层约定
- 产品语义：作者可从任意非终态删除（含审核中），故 `PENDING_REVIEW → DELETED` 合法；该迁移由单测先行暴露缺失后补入状态机表
- 状态由自定义类型 `Status` 承载，编译期防拼错

**代价**：
- 每个迁移需要 from 状态，调用方必须知道当前状态（Delete 需先查）

**V2 演进条件**：
- 引入审核队列后增加 PROCESSING → PENDING_REVIEW 的真实异步迁移

### ADR-009：统一视频可见性策略

**上下文**：视频有 PUBLISHED/PENDING_REVIEW/DELETED 等状态，不同接口需要一致的回答"谁能看到什么"。

**候选方案**：
1. 各 handler 自行判断
2. service 集中实现策略，任何不可见一律 ErrNotFound（选定）

**决策**：方案 2。

**理由**：
- 不泄露存在性：未发布/已删除对无权限者返回 404 与"不存在"同语义，无法枚举
- DELETED 对所有人（含作者）不可见——"删除 = 不存在"；该语义由 E2E 抓到的 bug（删除后作者仍可见）驱动补全
- 策略只实现一次，Feed 查询、详情、将来的推荐系统复用同一语义
- 所有可见性检查集中在 service 的 Get 方法内，不散落 handler

**代价**：
- 接口语义统一为"拿不到就是不存在"，无法区分"未发布"和"已删除"（这是刻意的）

### ADR-010：测试分层策略与 E2E 的必要性

**上下文**：M1 期间 fake repo 单测全绿，但 E2E 真库验收抓出两个单测测不出的 bug：`column` 自定义类型拼接破坏 gorm SQL 生成（整条条件被当参数值绑定）；NoSuchKey 被分类为 500 而非 400；删除后作者仍可见。

**候选方案**：
1. 仅单测（fake repo / fake storage）
2. 单测 + 里程碑 E2E 验收（选定）

**决策**：方案 2。

**理由**：
- fake repo 测业务分支（错误映射、状态机、凭证校验），但不经过 SQL 生成、不经过真实错误分类（minio NoSuchKey）
- E2E 用真实 Postgres + MinIO 跑完整上传时序，验证协议边界语义：错误码分类、HTTP 状态、可见性
- 每个里程碑结束时跑一遍 E2E 是"写测试证明问题存在"工作方式的落地点
- 单测断言用 require.ErrorIs 而非 Equal，验证错误包装链而非表面相等

**代价**：
- E2E 依赖本地容器环境，无法进 CI 单测流水线（可后续用 Testcontainers 自动化）

**V2 演进条件**：
- 引入 CI 后可用 Testcontainers 把 E2E 自动化；新模块上线前必须过一遍 E2E 验收

### ADR-011：Refresh Token 黑名单 + 旋转（无状态 JWT 的可撤销性）

**上下文**：JWT 无状态（ADR-001）的代价是"签发后无法主动失效"。M4 前 login/register 签发 refresh token 但没有 /refresh 消费方，logout 不存在——token 泄露则 7 天内可无限使用。

**候选方案**：
1. 纯无状态（现状）：无法登出，泄露不可撤销
2. 黑名单只拉黑 refresh token（选定）
3. per-user 当前 jti 白名单（只认最新签发的一个）

**决策**：方案 2 + refresh token rotation；**access token 不进黑名单**。

**理由**：
- access 15min 短命，为它查 Redis 牺牲无状态 + 每个请求一次额外往返（feed 12k QPS 场景是纯开销）；工业标准是 access 短 TTL + refresh 可撤销
- 黑名单 key `blacklist:refresh:{jti}`，**TTL = token 剩余寿命**，到期自动消失——零清理任务，"TTL 即生命周期"
- `SET key val EX ttl NX` 单命令原子：NX 防止重复 Block 缩短已有更长的 TTL
- rotation 顺序：**先拉黑旧 jti 再签发新 token**——两步骤间崩溃的后果是旧 token 作废、用户重登，而不产生新旧双活（无事务下的安全优先排序）
- 幂等：logout 收到无效/过期/类型不符的 token 一律视为已登出（成功 no-op），只有 Redis 故障才报错
- 顺带修复：软删用户（000002）的 refresh token 必须作废——Refresh 时查用户存在性，ErrNotFound → 401

**代价**：
- 并发用同一 token 刷新时可能都通过检查各自签发（短暂双活窗口，TOCTOU）
- logout/refresh 依赖 Redis 可用性（Redis 挂则无法登出，只能等 TTL）

**V2 演进条件**：
- 需要多设备管理/强制下线时切换方案 3（白名单存 per-user 当前 jti，天然解决并发双活）

### ADR-012：Feed 首页缓存策略（Cache-Aside + singleflight + double-check）

**上下文**：100 万数据下无缓存 feed 6,576 QPS（7 行基线 12,219 是假象，索引深度随数据量增长），db_pool_wait_count 5.8 万，25 连接饱和。M4 引入 Redis 缓存读路径。

**候选方案**：
1. Redis ZSET 缓存整个已发布视频有序列表，分页在缓存层做
2. 只缓存"默认 limit + 无游标"的第一页 JSON（选定）
3. 每页独立 key 缓存

**决策**：方案 2，Cache-Aside + 先写库再删缓存（ADR-005）+ singleflight + singleflight 内二次检查（double-checked locking）。

**理由**：
- 流量长尾：首页 QPS 占绝大多数，深层翻页命中率低不值得缓存；ZSET 方案有 score 精度问题（`(published_at,id)` 复合键装不进 double）和详情批量回源（fanout）复杂度，记入 V2
- 缓存的是 **service 拼完 URL 的产物**（FeedResult），repo 层缓存不了——缓存代码必须在 service 之后
- 自定义 limit 与缓存内容绑定，混用会串数据，故只缓存默认 limit
- **singleflight 只合并时间上重叠的调用**：缓存刚过期瞬间仍有缝隙，压测抓出的 flaky 证明其存在——必须二次检查（等待期间别人可能已回填），这才是防击穿的完整形态
- 缓存故障（Redis 挂）**降级直连 DB**，缓存不是单点故障；Set 失败只影响命中率，不影响本次响应
- 失效走消费侧接口：video 定义 `FeedCacheInvalidator`，main 装配 feed 的实现——video 不知道 feed 存在（模块边界不破）
- 失效失败只 Warn 不回滚：DB 已写入，脏窗口由 TTL（60s）兜底
- 压测验证：1M 数据无缓存 6,576 QPS / 缓存后 11,337 QPS（1.72x），db_pool_wait 归零

**代价**：
- 深层翻页不缓存；生产真实命中率取决于首页流量占比（压测 99.999% 是 10s 全首页流量的假象）
- TTL 周期内（最多 60s）发布/删除后首页可能短暂滞后
- 缓存后新瓶颈是 Redis GET + JSON 反序列化（~11.3k QPS 封顶）

**V2 演进条件**：
- 首页流量占比下降 / 需要覆盖翻页时：ZSET 列表缓存 + 详情 fanout
- 命中路径直接返回预序列化 bytes，省掉每请求 Unmarshal

### ADR-013：数据库约束取舍原则（FK/CHECK 的边界）

**上下文**：对"数据库尽可能轻、尽可能少约束"诉求的裁定。两类"轻"含义相反：不加多余的（对）vs 尽量少约束（错）。

**决策**：**唯一约束不可删**（幂等的地基，M3 的 ON CONFLICT 依赖冲突目标）；**CHECK 保留**（写入时零成本，防不可能状态）；**FK 按表评估**，不搞一刀切。

**评估标准**（三个变量）：
1. 写热不热（子表 INSERT 对父表行加共享锁，热行上锁竞争是删 FK 的真实动机）
2. 孤儿行可否容忍（有无生命周期/对账任务消化）
3. 当前量级下成本是否可测量（不可测量则保留完整性保证）

**逐表结论**：
| 约束 | 判定 | 理由 |
|---|---|---|
| follows/likes PK、users 唯一键 | 留 | 幂等（ON CONFLICT） |
| CHECK 不自动关注 | 留 | 零成本防不可能状态 |
| videos.author_id、follows.* FK | 留 | 低写频，防孤儿 |
| likes.video_id FK | 留（观察） | 热写路径；翻转条件见下 |
| upload_tickets.user_id FK | 唯一可讨论删 | ticket 是瞬态凭证，过期即废，孤儿由 expiry 生命周期消化 |

**翻转条件**（触发时才删，删后必须有兜底）：
- likes 单表写入达每秒几千行、压测出现锁等待/死锁 → 删 FK + 应用层校验 + 对账任务（如定时清理孤儿 likes）
- 分库分表时 FK 天然失效，需提前断

**代价**：
- 保留 FK 在超高写入量级会有限制；当前量级（每秒个位数写）成本为 0
- 任何约束的删除必须是 ADR 决策（写明代价与兜底策略），不是审美偏好
