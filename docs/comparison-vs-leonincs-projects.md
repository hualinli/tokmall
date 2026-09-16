# 技术对比报告：tokmall vs feedsystem_video_go vs GCFeed

> 静态代码分析报告，未运行任何项目（无依赖安装、无编译、无压测）。
> 分析对象：
> - **tokmall**：`~/Projects/tokmall`（本仓库）
> - **feedsystem_video_go**：https://github.com/LeoninCS/feedsystem_video_go.git（下文简称 **FSVG**，commit `7c1c27d`）
> - **GCFeed**：https://github.com/LeoninCS/GCFeed.git（commit `8cf995c`；其 README 自述为 FSVG 的"更全面完整"迭代版）
>
> 所有结论均给出代码位置证据。行数统计口径：非测试 Go 代码 / 测试 Go 代码。

---

## 0. TL;DR

| | 一句话定位 |
|---|---|
| **FSVG** | 功能广度优先的短视频系统练手作：评论/私信/SSE/分片上传/话题标签都有，但架构边界松（具体类型依赖、AutoMigrate、log.Printf），缓存设计有巧思也有硬伤 |
| **GCFeed** | 同一作者的架构升级版：DDD 四层 + 策略模式 + 消费侧接口；互动走"Redis 快路径 + MQ 异步落库"，关注流推拉混合，甚至做了基于 embedding 余弦相似度的推荐链路——三者中**异步化和推荐深度最强** |
| **tokmall** | 工程纪律优先的模块化单体：契约驱动（OpenAPI+route-sync 测试）、schema 治理（golang-migrate）、安全细节（HMAC 游标、upload_ticket、账号维度限流）、错误三层链、以及**用压测数据驱动技术决策**（Kafka 缓期、缓存收益量化）——三者中**工程治理与安全设计最强，业务广度最窄** |

三者的共同底座是 Gin/GORM(Hertz)/MySQL(PG)/Redis/RabbitMQ(可选) 单体。差异本质不在"用了什么组件"，而在**边界怎么划、一致性怎么保证、决策有没有证据**。

---

## 1. 项目定位与规模

| 维度 | FSVG | GCFeed | tokmall |
|---|---|---|---|
| 后端非测试代码 | ~7.2k 行（backend）+ 2.3k 行（内嵌 simple-feed） | ~13.0k 行（api + worker） | ~5.9k 行 |
| 测试代码 | backend ~530 行 + simple-feed ~1.5k 行 | ~5.4k 行（13 个 API 流程测试文件 + 单测） | ~4.0k 行（14 文件）+ 前端 Vitest/Playwright e2e |
| 前端 | Vue 3 + Pinia + spark-md5（分片上传客户端） | React 18 裸写（无路由/状态库） | React + TanStack Router/Query + openapi-typescript 生成类型 |
| 业务域 | 账号/视频/点赞/评论/关注/Feed/私信/通知/话题标签 | 账号/视频/点赞/**收藏**/评论/关注/Feed/**推荐/曝光/播放优化/消息中心** | 账号/视频/点赞/关注/Feed/播放事件 + **商品/SKU/库存**（Commerce v1 已起步） |
| 部署形态 | docker-compose + start.sh（API 与 Worker 分进程） | docker-compose 全家桶（含 Prometheus/Grafana） | make dev（依赖容器 + 后端 + 前端） |

值得注意的演化线索：FSVG 仓库里内嵌了一个 `simple-feed/` 子项目（Gin + PostgreSQL），它已经用上了消费侧接口（`simple-feed/internal/video/service.go:16 type VideoRepo interface`）和 gomock——说明作者在 FSVG → GCFeed 的过程中意识到了接口边界问题，GCFeed 全面落实了这一点。

---

## 2. 技术栈总览

| 组件 | FSVG | GCFeed | tokmall |
|---|---|---|---|
| HTTP 框架 | Gin | Gin | **Hertz v0.10**（hertz-contrib requestid/monitor-prometheus/logger-slog） |
| ORM / DB | GORM / MySQL | GORM / MySQL | GORM / **PostgreSQL** |
| Schema 管理 | **GORM AutoMigrate** | **GORM AutoMigrate**（带 1060/1061 并发重试） | **golang-migrate 版本化 SQL 迁移**（明令禁止 AutoMigrate） |
| 缓存 | Redis（可选降级）+ 进程内 go-cache（L1） | Redis（页/卡片/计数/热榜/收件箱） | Redis cache-aside + singleflight + double-check + TTL 抖动 |
| MQ | RabbitMQ（like/comment/social/popularity/timeline 五条 topic，可降级直写） | RabbitMQ（互动落库/视频发布 fanout/embedding 任务） | **无 MQ**（Kafka 经压测评估后明确缓期，见 performance-baseline） |
| 对象存储 | 本地磁盘 `.run/uploads/` | 本地磁盘 `uploads/` + ffprobe/ffmpeg 校验转封装 | **MinIO 预签名 PUT 直传 / GET 播放** |
| 认证 | JWT access+refresh（Redis+DB 双存） | JWT access（单 token，logout 删状态） | JWT access 15min/refresh 7d + **Redis 黑名单可撤销**（ADR-011） |
| 限流 | 固定窗口 INCR+EXPIRE，IP/账号，fail-open | 无（README 把"系统治理"列为待补） | 固定窗口 **lua 原子** INCR+EXPIRE，三档规则 + **账号维度提取器**，fail-open |
| 可观测 | pprof + healthz，log.Printf | **Prometheus（API+Worker 双端点）+ Grafana 面板 + k6 压测文档** | Prometheus :9091 + db_pool/feed_cache 自定义 collector + slog 结构化日志 + lumberjack |
| API 契约 | Postman Collection | 无（REST 风格文档约定） | **OpenAPI 3 embed + route-sync 防漂移测试 + TS 类型生成** |

---

## 3. 架构与分层

### FSVG：模块分层但依赖具体类型
- 目录按业务模块切（account/video/feed/social/message），每模块 handler/service/entity/repo 齐全，结构清晰。
- 但 service 直接持有 `*FeedRepository`、`*video.LikeRepository` 等**具体 struct 指针**（`internal/feed/service.go:20-26`），没有消费侧接口 → 无法 mock、模块间编译耦合。
- 错误处理以 `gin.H{"error": ...}` 字符串为主，没有统一错误码层。

### GCFeed：DDD 四层，三者中最完整
```
interfaces/http（handler/router/middleware）
   ↓
application/*（用例服务，策略模式装配，Option 注入依赖）
   ↓
domain/*（实体 + Repository 接口 + 领域错误 —— 接口定义在消费侧 ✅）
   ↓
infra/*（cache/database/jwt/mq/persistence/metrics 实现）
```
- `application/feed/service.go` 用 **scene 策略注册表**分发 timeline/hot/following/recommend 四种 Feed（`Strategy` 接口 + `RegisterStrategy`），新增场景不改主流程——这是教科书式的开闭原则落地。
- 领域层有真正的领域行为（`domainfeed.RestoreFeedItem`、`domaininteraction.NormalizeActionType`），不是贫血模型。
- 小瑕疵：`infra/cache/feed_cache.go` import 了 `applicationfeed` 和 `applicationinteraction`（基础设施反向依赖应用层，为复用 DTO），严格 DDD 眼光属于层次倒置。

### tokmall：模块化单体 + 显式架构约定
- 模块间不共享类型、不互相 import service，只读共享表时各自写 read model SQL（`internal/inventory/repository.go` 中 `FindSkuOwner` 注释明确记录了这一决策）。
- 错误三层链（repo 哨兵 → service 分类分级日志 → handler BizError 映射，未知一律 500）是三个项目中唯一把"日志分级"写进架构约定的。
- 列名/Status 用自定义类型防拼错，model 永不出 service 层。
- 弱点：没有 GCFeed 那样的 domain 行为层，service 偏过程式；但换来的是每个模块边界清晰、可独立拆分。

**结论**：分层成熟度 GCFeed ≈ tokmall > FSVG。GCFeed 赢在领域建模与策略模式，tokmall 赢在跨模块边界纪律。

---

## 4. 数据库与 Schema 治理

这是差距最大的一维。

- **FSVG / GCFeed 都在启动时 AutoMigrate**。GCFeed 甚至写了并发迁移冲突重试（`migration.go:33-46`，捕获 MySQL 1060/1061 后 sleep 重试 4 次），并在注释里自认"适合教学项目快速启动"。问题在于：
  - AutoMigrate **不会删列/改列类型**，schema 漂移不可审计；
  - 多实例同时启动时 DDL 竞态只能靠重试碰运气；
  - 迁移历史不存在，无法回滚。
- **tokmall 用 golang-migrate + 手写版本化 SQL**（`migrations/000001..000009`，含 down 迁移），部分索引、条件更新等精细控制都写在 SQL 里（如 002 软删、005 作者索引）。Commerce 迁移（007-009）还演示了约束重构流程（drop check constraints → rename column）。
- 三者共同点：都没有处理分布式 schema 变更协调（这对单体都够用）。

**结论**：tokmall 的做法是唯一可平移到生产团队的。GCFeed 作者显然知道这点（注释自辩），属于有意识的取舍而非盲区。

---

## 5. 认证与安全

| 项 | FSVG | GCFeed | tokmall |
|---|---|---|---|
| Token 模型 | access + refresh，refresh 存 Redis+DB，改名/改密强制失效 | 仅 access JWT，logout 靠前端丢 token（服务端无撤销） | access 15min / refresh 7d 旋转 + logout 黑名单 |
| 密码哈希 | bcrypt | bcrypt | bcrypt（登录 ~85 QPS 是防爆破的设计代价，有 ADR 记录） |
| 上传安全 | multipart 直传服务器，扩展名/MIME 白名单 | 同左 + ffprobe 校验时长/分辨率/编解码器 + ffmpeg faststart（`interfaces/http/upload/handler.go` 白名单表） | **upload_ticket 状态机**：预签名 URL 与票据绑定核销，防伪造直传（`internal/video/service.go:83 statusTransitions` + DB 条件更新 `WHERE status=from`） |
| 游标防篡改 | 无签名（时间戳裸奔） | 无签名（base64 JSON 可伪造 published_at/offset） | **HMAC-SHA256 签名 + rank_version 内嵌**（`internal/feed/cursor.go:38`），篡改即 400，换排序旧游标自动失效 |
| 可见性语义 | 未发布视频未见显式过滤说明 | Get 只过滤已发布 | DELETED 对所有人 404、未发布仅作者可见、一律 ErrNotFound 不泄露存在性 |
| 内部接口保护 | 无内部接口 | `/internal/*` 端点有独立 InternalTokenAuth 中间件 ✅ | 无内部接口 |

**亮点对比**：GCFeed 的上传内容校验（时长 ≤10min、分辨率 ≤3840、编解码器白名单、faststart）是三者中最专业的媒体处理；tokmall 的 upload_ticket + 条件更新状态机是三者中最严谨的上传授权模型。两者恰好互补。

---

## 6. 视频上传与存储链路

- **FSVG**：普通上传（200MB 上限）+ **5MB 分片上传**（`video/chunk_handler.go`）：Redis 会话存 uploaded bitmap，单片 MD5 校验，断点续传（同 user+file hash 复用会话），complete 时服务端顺序合并落本地盘。客户端用 spark-md5 算 hash。这套东西功能完整，但存储是本地磁盘——**多实例部署即失效**，且合并期间占用 API 进程 IO。
- **GCFeed**：单次 multipart 直传本地盘，但服务端做 ffprobe 元数据校验 + faststart 转封装（对播放体验有真实价值），并支持 Range 请求（有专门测试 `test/upload_static_range_test.go`）。同样受限于本地磁盘。
- **tokmall**：预签名 PUT 直传 MinIO，API 不经手文件字节；播放走预签名 GET。天然水平扩展，代价是需要对象存储组件。

**结论**：功能丰富度 FSVG > GCFeed > tokmall；生产正确性 tokmall > GCFeed > FSVG。FSVG 的分片上传若叠加对象存储 multipart API 会更有意义。

---

## 7. Feed 读路径与分页游标（核心差异之一）

### FSVG：冷热分离 ZSET 时间线
`internal/feed/service.go ListLatest`：
- Redis ZSET `feed:global_timeline` 由 MQ 消费者维护（cap 1000 条，`worker/outboxworker.go StartConsumer` 里 `ZRemRangeByRank 0,-1001`）；
- 读时取 ZSET 尾部 score 作为**水位线**：请求游标新于水位 → 纯 Redis `ZRevRangeByScore`；旧于水位 → 降级查 MySQL（带 per-cursor singleflight）；跨界 → 热+冷拼接；
- ZSET 空（如 Redis 重启）→ 全局静态锁 singleflight 回源重建最新 1000 条，所有阻塞请求重试。

思路很漂亮，但有三个静态可见的问题：
1. **P1 游标只有时间戳**（`NextTime = last.CreateTime.UnixMilli()`）：同一毫秒的多条视频会翻页重复/丢失，且游标无签名无 tie-breaker ID；
2. **P1 L3 回源是 N+1**：`GetVideoByIDs` 对每个 miss 的 ID 各起一个 goroutine 各查一条 SQL（`for _, id := range missedL2 { go func(){ f.repo.GetByIDs(ctx, []uint{videoID}) } } }`），缓存击穿时 DB 承受 N 倍 QPS；回写还是 fire-and-forget goroutine（context.Background()，无上限）；
3. 关注流响应缓存 **TTL 24h**（`cacheTTL: 24 * time.Hour`）——点赞数最长陈旧一天。

### GCFeed：轻量页 + 卡片/计数分离组装
`application/feed/service.go`：
- 页缓存只存**轻量 FeedPageItem**（videoID/authorID/publishedAt/hotScore），卡片与计数在组装阶段批量 `MGET video:card:*` / `video:stat:*`，缺失批量回源再回填——**读放大被精确控制为两次 MGET + 至多两条批量 SQL**，这是三者中缓存粒度设计最好的；
- 页缓存 singleflight + **组内 double-check**（进锁后再查一次缓存）+ **TTL 抖动**（fnv hash 10–20% jitter，`feedPageCacheTTL`）；首页 5s / 深页 45s 差异化 TTL；
- 计数缓存三级兜底：JSON 快照（15s TTL）→ base 计数（HSETNX 从 DB 初始化）→ **16 分片 HINCRBY** 合并（`interactionStatCounterShardCount=16`，按 userID 分片写、pipeline 并读）——热点视频计数写竞争被显式打散，这是三者中唯一的 hot-key 工程手段。

### tokmall：SQL 直查 + 签名游标 + Ranker 抽象
- 全局 Feed：`(published_at DESC, id DESC)` + 部分索引，游标 `(published_at,id)` HMAC 签名 + rank_version（排序演进时旧游标自动失效，`ranker.go Version()` 机制）；
- 关注 Feed：**窗口函数一次 SQL 完成"每作者 TOP K"**（`repository.go:80 ROW_NUMBER() OVER (PARTITION BY v.author_id)`），无 N+1，游标下推 + 空关注短路（实测 20k QPS）；
- Ranker 接口把召回与排序分离；TimeRanker 是当前生产排序，HotRanker 保留为无在线分页的单元实验（配置 fail-fast 禁止启用），HybridRanker 待做。

**结论**：GCFeed 的缓存组装架构 > tokmall 的 SQL 简洁性 > FSVG（N+1 硬伤）。但游标安全性上 tokmall 独一档——两个 LeoninCS 项目的游标都可被客户端任意构造。

---

## 8. 关注 Feed：纯拉 vs 推拉混合

| | FSVG | GCFeed | tokmall |
|---|---|---|---|
| 模式 | 纯拉（JOIN follows），整响应缓存 24h | **推拉混合**：粉丝 <10000 人 → 发布事件 fanout 到每个粉丝 inbox（ZSET，批 500，cap 1000 条）；≥10000（`BigCreatorFollowerThreshold`）→ 只写作者 outbox，读时拉 | 纯拉：窗口函数每作者 TOP 5 + 首页缓存 |
| 读路径 | DB JOIN | pipeline 合并 viewer inbox + 所关注作者的 outbox（ZRevRangeByScore + 去重 + 内存排序） | 窗口函数 SQL（或命中页缓存） |
| 大 V 问题 | 无特殊处理 | 显式阈值分流 ✅ | 无特殊处理（靠缓存抹平，实测 1000 关注无缓存仅 204 QPS） |

GCFeed 这块是三者最完整的工业模式（推拉结合 + 容量上限截断 + 发布预热点卡）。tokmall 的 mentor 路线是"先测量再引入复杂度"——performance-baseline.md 记录了拉模式在 1000 关注时的超线性退化及缓存后的抹平效果，属于有数据支撑的不同取舍。诚实评价：如果关注数继续增长，GCFeed 的结构比 tokmall 当前结构更抗造。

---

## 9. 热榜实现：同一作者的两代方案 vs tokmall 占位

- **FSVG**：60 个分钟桶 ZSET + `ZUnionStore` 合并到 per-minute 快照 key（2min TTL）+ offset 翻页（as_of 冻结窗口保证翻页稳定）。DB 兜底路径还有 `(popularity, create_time, id)` 复合游标。
- **GCFeed**：几乎同一套设计，但更收敛：分钟桶由互动事件驱动（like=3/favorite=4/comment=5 权重加权 `ZIncrBy`，桶 TTL 2h），快照重建前先 `Exists` 检查，热榜游标冻结 `window_end` + offset（`HotCursor.WindowEnd`），并清理 `<=0` 分数成员。
- **tokmall**：HotRanker 基于 play/like 计数排序，作为 Ranker 单元实验存在；当前排序键未与时间游标闭合，不能作为生产深分页热榜。无 Redis 分钟桶（M8 边界已收口，HybridRanker 待做）。

**结论**：热榜是 FSVG/GCFeed 相对 tokmall 最明确的领先点。tokmall 若做 HybridRanker，分钟桶 + 快照冻结窗口是现成可抄的参考实现（注意补上 GCFeed 缺的签名游标）。

---

## 10. 互动链路（点赞幂等与异步化）

三种截然不同的一致性策略：

1. **tokmall（同步强一致）**：唯一约束 + `ON CONFLICT DO NOTHING`，结果语义固定（like→true）。简单、可验证、并发安全（M3 有并发测试）。写路径直接打 DB。
2. **GCFeed（Redis 快路径 + MQ 异步落库）**（`application/interaction/service.go setActionAsync` + `infra/cache/feed_cache.go SetActionState`）：
   - 先读 DB 拿初始计数 → Redis `WATCH(actionKey)` 事务里比对 idempotency_key 与 status 计算 delta → TxPipeline 写 action hash + 分片计数；
   - delta≠0 才发 MQ（`ActionChangedEvent` 带 event_id），**MQ 发布失败自动回退同步直写 DB** ✅；
   - Worker（`application/interaction/worker.go`）消费落库。
   - 静态风险（P1）：action 状态与分片计数都是 Redis 易失数据（TTL 30d/24d），若 Redis 在 Worker 落库前丢失（故障/驱逐），**delta 永久丢失**且无对账机制；WATCH 重试逻辑在代码里也未见到重试循环（Watch 返回乐观锁冲突错误时直接映射为 `ErrUpdateInteractionFailed`）。
3. **FSVG（MQ-first + 失败降级直写）**：like 请求优先发 `like.events`，失败则同步写 DB；LikeWorker 重试 3 次（指数退避）后 **Ack 丢弃消息**（`likeworkerower.go handleDelivery` maxRetries=3 → Ack）——丢弃意味着 likes_count 可能永久漂移（P1），好在设计文档提到 DLX，但 worker 代码路径里未见 DLX 消费者。

幂等键使用广度：GCFeed 给点赞/收藏/评论/视频创建/QoS 上报全都配了 Idempotency-Key（含"先查后插、冲突再查"的双检模式，`CreatePublished` 写得很规范）；tokmall 用服务端生成的 UUIDv7 event_id（play_events）与关系表唯一约束，不依赖客户端幂等键；FSVG 幂等主要靠唯一约束。

---

## 11. MQ 与 Worker 体系

- **FSVG**：五条 topic + DLX 声明；**Outbox 模式**（`outboxworker.go`：发布事务内写 outbox 行 → poller 轮询 pending → 发 MQ → 成功后删行）。这是三者中唯一的事务性发件箱实现——值得肯定，尽管"发成功后删行"在崩溃窗口仍可能重复投递（at-least-once，消费者需幂等，timeline ZADD 天然幂等所以实际安全）。SSE 通知 hub 是进程内的（重启丢连接、多实例不互通，P2）。
- **GCFeed**：三条异步管线全部跑在独立 worker 进程（`:9091` 独立 metrics）：① 互动落库；② 视频 fanout（含卡片预热 PreheatFeedVideo——发布即预填 card/stat 缓存，冷启动首刷不打 DB，好设计）；③ embedding 生成。API↔Worker 通过 `/internal/*` + InternalTokenAuth 回调。
- **tokmall**：无 MQ。Kafka 异步化经压测证明当前同步写不是瓶颈后**主动搁置**并写入里程碑表（M5-Kafka 搁置）——这是"出现可测量问题再引入技术"原则的直接体现，与另外两位"先把 MQ 架起来"形成方法论对照。两种路线各有道理：学习目的下 GCFeed 收获的是异步系统的实战坑，tokmall 收获的是决策依据本身。

---

## 12. 推荐系统：GCFeed 独有的一整条链路

`application/recommendation/service.go`（448 行）+ `embedding/` + `exposure/`：

- **召回**：候选池 = limit × 8（50–500 封顶）；
- **排序**：freshness 时间衰减 + **用户兴趣向量与视频向量的余弦相似度**（向量来自 HashNgramVectorizer 对标题/描述的分词哈希，`domain/embedding/hash_ngram*`）+ 作者去重交错（`interleaveByAuthor`）；
- **曝光去重**：7 天窗口（`RecentExposureWindow = 7*24h`）内已曝光候选降权/过滤；曝光通过 `/internal/exposures` 回调写入；
- **画像闭环**：观看事件（`POST /video-view-events`）→ MQ → worker 更新用户兴趣向量；视频发布 → MQ → embedding worker 生成向量。

坦率的技术评价：HashNgram 向量是玩具级语义表示（无泛化能力），per-request 加载 500 候选 + 全量向量余弦也没有 ANN 索引——**但整条"行为闭环→特征→排序→反馈"的数据流骨架是真的**，这在个人项目里罕见。tokmall 对应位置是 Ranker 接口 + TimeRanker 生产分页 + HotRanker 单元实验（M8 明确稳定热度游标与 HybridRanker 待做）。

---

## 13. 限流与治理

- **FSVG**：固定窗口 INCR+EXPIRE，IP/账号两维度，Redis 故障 fail-open（`ratelimit.go` err → c.Next()）。非原子 INCR+EXPIRE 有竞态（key 可能永不过期，除非用了 lua——其 `IncrementWithExpire` 实现在 redis middleware 中，静态看是分开调用，P2）。
- **GCFeed**：无限流（README 自己把"系统治理"列为待补能力）。
- **tokmall**：lua 脚本原子 INCR+条件 EXPIRE（`middleware/ratelimit.go:116`）；三档规则（auth 10/min、read 200/10s、write 100/10s）；auth 场景从请求体提取账号标识限流（NAT 下按 IP 会误伤，`AccountKeyExtractor` 注释写明了动机）；fail-open 降级；429 映射 RATE_LIMITED 错误码。

治理维度 tokmall 完胜，GCFeed 缺位，FSVG 及格线。

---

## 14. 可观测性

| | FSVG | GCFeed | tokmall |
|---|---|---|---|
| 日志 | log.Printf（非结构化，中文混杂） | log 标准库为主 | slog 结构化（stdout + lumberjack 轮转），requestid 贯穿 |
| Metrics | 无 | **Prometheus 双端点**（api /metrics + worker :9091），自定义 feed_request_p95、cache hit、mysql_query_p95、worker job duration；Grafana 面板随 compose 启动 | Prometheus :9091 独立端口 + db_pool / feed_cache 自定义 collector |
| Profiling | pprof（6060/6061）✅ | 无 | 无 |
| 压测文化 | 无文档 | **k6 脚本 + 压测方法论文档**（docs/performance-testing.md，含极限 QPS 测法与指标解读） | wrk + docs/performance-baseline.md（含逐轮数字与结论：12k→6.6k→11.3k QPS、关注流 204→6.6k） |

GCFeed 的监控栈完整度最高；tokmall 与 GCFeed 都有"记录基线数字"的习惯，FSVG 缺失。GCFeed 文档里的示例数字（19.85 QPS @ 20 VU think-time）规模远小于 tokmall 基线的万级 QPS——两边测的目标不同（功能验证 vs 容量边界），不宜直接比较绝对值。

---

## 15. 测试与契约

- **FSVG**：backend 测试覆盖薄（~530 行，集中在 chunk upload 和 redis）；simple-feed 子项目反而质量高（gomock + 表驱动，~1.5k 行）。无 API 契约，Postman collection 充当调试工具。
- **GCFeed**：13 个端到端 API 流程测试（`apps/api/test/*_api_test.go`，覆盖 account/video/upload/feed/recommendation/message/playback/relation/exposure）+ cache/domain 单测，共 ~5.4k 行——**测试行数超过 tokmall 且接近代码量一半**，是其工程质量最强的证据之一。但无 OpenAPI 契约，路由与文档手工同步。
- **tokmall**：14 个后端测试文件（~4k 行，testify + hertz ut 内存链路）+ **route-sync 测试防路由漂移** + 前端 Vitest + Playwright e2e + OpenAPI embed。契约驱动让前后端并行开发成为可能（openapi-typescript 生成类型）。

**结论**：GCFeed 测试深度 ≈ tokmall 测试体系广度，各擅一类；FSVG 明显薄弱。

---

## 16. 各自问题清单（静态审查发现，按 P0/P1/P2）

### FSVG
- **P1** 最新 Feed 游标仅时间戳（毫秒碰撞重复/丢页、无签名、无 tie-breaker ID）— `feed/service.go ListLatest`
- **P1** `GetVideoByIDs` L3 回源 N+1（per-ID goroutine + 单条查询）+ 无界 fire-and-forget 回写 goroutine
- **P1** 关注流响应缓存 TTL 24h（计数陈旧一天）
- **P1** LikeWorker 3 次重试后 Ack 丢弃消息（计数永久漂移风险）
- **P2** service 依赖具体类型（不可 mock）、AutoMigrate、log.Printf、SSE hub 进程内有状态
- **P2** compose 默认 JWT_SECRET 写死在文档（`feedsystem-dev-secret-key`）
- 亮点：Outbox 模式、冷热分离水位线、分片上传断点续传、MQ 可选降级的容错思路

### GCFeed
- **P1** 游标全部未签名（base64 JSON 可伪造 published_at/offset/rank_score）
- **P1** 点赞快路径的 Redis 易失状态（action 30d / 分片计数 24d TTL）在 Worker 落库前丢失则 delta 丢失，无对账/补偿；WATCH 冲突未见重试
- **P1** AutoMigrate 启动执行（作者自知，注释自辩为教学取舍）
- **P2** 关注流读路径每请求 `ListFollowingPullAuthorIDs` 全量加载关注列表 + N 个 outbox key pipeline（万级关注会退化；大 V 阈值分流只解决了写侧）
- **P2** 推荐每请求加载最多 500 候选 + 全量余弦（无 ANN、无缓存），QPS 上限低
- **P2** 本地磁盘上传限制水平扩展；infra 反向 import application 层
- 亮点：16 分片计数器、TTL 抖动、推拉混合 fanout、发布预热、embedding 推荐闭环、ffprobe/faststart 媒体校验、InternalTokenAuth、双进程 metrics

### tokmall（自我审视）
- **P1** 无评论/收藏/私信/通知域，业务完整度低于两个对照项目
- **P1** 关注 Feed 无缓存的拉模式在千级关注已实测退化（204 QPS），推拉混合/HybridRanker 未做
- **P2** play_events 同步写在突发写峰值下是潜在热点（已有 Kafka 决策记录兜底）
- **P2** 无 pprof；无内部回调鉴权机制（未来 Commerce 支付回调需要）
- 亮点：HMAC+version 游标、upload_ticket 状态机、golang-migrate、OpenAPI route-sync、lua 限流 + 账号维度、错误三层链、压测驱动决策、Commerce 起步

---

## 17. 互相可借鉴清单

### tokmall 可以拿来的
1. **GCFeed 的卡片/计数分离缓存组装**（轻量页 + 批量 MGET + 批量回源）——比整页 JSON 缓存的计数新鲜度好得多，可直接用于 Feed 计数展示。
2. **GCFeed 的分钟桶热榜 + window_end 冻结游标**——HybridRanker 的现成参考，记得加 HMAC。
3. **GCFeed 的发布预热（PreheatFeedVideo）**——视频发布事件顺手填卡片缓存，成本一行。
4. **GCFeed 的 API 流程测试组织**（test/ 包按域拆端到端文件）——补充 hertz ut 之外的整链路视角。
5. **FSVG 的 Outbox 模式**——将来 Commerce 订单事件/Kafka 化时的标准答案，FSVG 的实现（含删行语义）可以直接讨论着抄。
6. **GCFeed 的 ffprobe/faststart 媒体校验**——MinIO 直传后服务端失去检查点，可在 publish 时对对象做异步校验。

### 两个 LeoninCS 项目可以拿走的（反向输出）
1. golang-migrate 替代 AutoMigrate（tokmall migrations 目录即模板）。
2. HMAC 签名游标 + rank_version 失效机制。
3. upload_ticket 式预签名直传，摆脱本地磁盘的单机约束。
4. lua 原子限流 + auth 场景账号维度提取。
5. OpenAPI 契约 + route-sync 测试 + 前端类型生成。
6. 结构化日志 + 错误三层链（GCFeed 目前错误全是 `errors.New("failed to load X")` 泛型哨兵，handler 侧丢失分类信息）。

---

## 18. 总结

这三个项目恰好构成一条学习曲线上的三个刻度：

- **FSVG** 解决了"功能能不能做出来"——分片上传、SSE、五种 MQ topic 都跑起来了，但以具体类型耦合和启动期 AutoMigrate 为代价；它的缓存热情（L1/L2/L3、冷热分离）大于它的缓存严谨性（N+1、24h 陈旧、时间戳游标）。
- **GCFeed** 解决了"架构能不能立住"——DDD 四层、策略模式、消费侧接口、推拉混合、异步管线、监控面板，是三者中**系统设计含量最高**的项目；剩余短板集中在安全细节（未签名游标、无撤销登出、无限流）和数据治理（AutoMigrate、易失计数无对账）。
- **tokmall** 解决了"决策能不能自证"——每一项技术引入/拒绝都有 ADR 或基线数字背书，契约、迁移、限流、可见性这些"不性感但生产致命"的环节做得最扎实；短板是业务域窄、异步化与推荐尚未真正展开。

如果用一句话给本仓库定位：**tokmall 的下一程（HybridRanker、Kafka 化、Commerce 订单）恰恰需要从 GCFeed 借结构、从 FSVG 借教训**——而对方项目要走向生产，第一课应该是 tokmall 的 migrations 目录和 cursor.go。

---

## 附录：关键证据文件索引

| 结论 | 证据位置 |
|---|---|
| FSVG N+1 回源 | `feedsystem_video_go/backend/internal/feed/service.go` `GetVideoByIDs`（per-ID goroutine + `repo.GetByIDs(ctx, []uint{videoID})`） |
| FSVG 时间戳游标 | 同文件 `ListLatest`（`NextTime = CreateTime.UnixMilli()`） |
| FSVG 24h 缓存 | 同文件 `NewFeedService`（`cacheTTL: 24*time.Hour`） |
| FSVG Outbox | `backend/internal/worker/outboxworker.go` |
| FSVG 丢弃消息 | `backend/internal/worker/likeworker.go` `handleDelivery`（maxRetries=3 后 Ack） |
| FSVG 分片上传 | `backend/internal/video/chunk_handler.go` |
| GCFeed 策略注册表 | `GCFeed/apps/api/internal/application/feed/service.go`（Strategy/RegisterStrategy） |
| GCFeed 页缓存+jitter | 同文件 `loadFeedPage`/`feedPageCacheTTL`（fnv 10–20% jitter、singleflight 组内 double-check） |
| GCFeed 分片计数/WATCH | `internal/infra/cache/feed_cache.go` `SetActionState`（shard=16、HSETNX base、TxPipelined） |
| GCFeed 推拉阈值 | `internal/application/video/fanout_worker.go` + `internal/domain/feed/entity.go:11`（threshold=10000） |
| GCFeed 推荐/曝光 | `internal/application/recommendation/service.go`（pool×8、cosine、interleaveByAuthor、RecentExposureWindow=7d） |
| GCFeed AutoMigrate | `internal/infra/persistence/migration/migration.go`（1060/1061 重试、"适合教学项目"注释） |
| GCFeed 媒体校验 | `internal/interfaces/http/upload/handler.go`（ffprobe 白名单/faststart） |
| tokmall HMAC 游标 | `internal/feed/cursor.go:38`（JSON→Base64URL→HMAC-SHA256） |
| tokmall 缓存纪律 | `internal/feed/service.go:185-198`（singleflight+double-check）、`internal/feed/cache.go:53-60`（jitterWindow=20s） |
| tokmall 限流 lua | `internal/middleware/ratelimit.go:103-117` |
| tokmall 状态机 | `internal/video/service.go:83`（statusTransitions）+ `model.go:39`（upload_tickets） |
| tokmall 窗口函数 | `internal/feed/repository.go:80-88`（ROW_NUMBER PARTITION BY author） |
| tokmall 迁移 | `migrations/000001..000009*.sql`（含 commerce 007-009） |
