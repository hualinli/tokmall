# Feed 系统 Mentor 手册

## 0. 你要完成什么

目标不是一次实现 TikTok，而是通过一个可演进的 Feed 系统，掌握以下后端能力：

- Go + Hertz HTTP 服务
- PostgreSQL 建模、索引和游标分页
- Redis 缓存、热点治理和限流
- Kafka 事件和异步 Worker
- 内容状态、可见性和幂等
- 压测、指标、Trace 和故障排查
- 从时间线演进到关注 Feed 和简单排序

最终演示链路：

```text
用户登录 -> 发布视频 -> Feed 拉取视频 -> 点赞/关注 -> 播放事件异步上报
                         |
                         -> 视频下方显示商品卡（为商城阶段预留）
```

第一版不做复杂推荐模型、不做真实转码、不做完整社交图谱。先把“返回正确的一页内容”做稳定。

## 1. 工作方式

每个问题都按以下顺序处理：

1. 写出可以复现的问题。
2. 选择最小可行方案。
3. 写测试或压测证明问题存在。
4. 实现解决方案。
5. 再测一次，记录数据和代价。
6. 写一页 ADR，说明为什么这样选。

不要先引入 Kafka、推荐模型或微服务。只有当一个可测量的问题出现时，才增加技术。

每个任务的完成定义：代码、测试、日志/指标、文档和可演示结果全部具备。

## 2. 总体架构

### 2.1 第一阶段：模块化单体

```text
Client
  -> Hertz API
      -> content service
      -> feed service
      -> interaction service
      -> PostgreSQL
      -> Redis（后续）
      -> MinIO（视频文件）
```

推荐目录：

```text
cmd/api/main.go
cmd/worker/main.go
internal/content/
  handler.go
  service.go
  repository.go
  model.go
internal/feed/
  handler.go
  service.go
  repository.go
  cursor.go
internal/interaction/
internal/event/
internal/observability/
```

Handler 只处理 HTTP；Service 处理业务规则；Repository 只访问数据库；事件发布和消费单独放置。

### 2.2 数据所有权

- `videos`、`follows`、`likes` 属于 Content/Interaction 模块。
- Feed 只负责生成候选内容和排序，不拥有视频事实数据。
- 播放次数等聚合数据可以异步写入，但原始事件要保留一段时间以便重放。
- 以后拆服务时，跨模块只传 `video_id`、`author_id`、`user_id`，不共享表。

## 3. 里程碑总览

> 状态：M0-M4 已完成（含基线压测，见 performance-baseline.md）；M4 之后实际还做了
> auth 闭环（/refresh 旋转 + /logout 黑名单）、公共主页/作者列表/社交列表、
> OpenAPI 契约 + React 前端 + Playwright e2e（详见 AGENTS.md 里程碑表）。
> **方向 1 已交付**：M5 同步版（play 端点 + event_id 幂等）、M6（关注 Feed 拉模式）、
> M8 基础（Ranker 接口 + HotRanker + 版本化 + 实验开关）。
> **下一步**：M5 Kafka 异步化（先有消费方再引 Kafka，见 §9 顺序问题）、M7 限流。

| 里程碑 | 解决的问题 | 主要技术 | 状态 |
|---|---|---|---|
| M0 | 服务能启动、可测试 | Hertz、PostgreSQL、Docker | ✅ |
| M1 | 视频能发布和查询 | MinIO、状态机 | ✅ |
| M2 | Feed 能正确分页 | 复合索引、游标 | ✅ |
| M3 | 用户能互动 | 唯一约束、幂等 | ✅ |
| M4 | 读流量增加后仍稳定 | Redis、singleflight | ✅ |
| M5 | 播放事件不阻塞接口 | Kafka、Worker | 🔶 同步版已完成，Kafka 待做 |
| M6 | 关注 Feed 可扩展 | 拉模式、批量查询、缓存 | ✅ 拉模式（窗口函数 TOP K） |
| M7 | 热点和异常可治理 | 限流、熔断、降级、观测 | ⏳ 待做 |
| M8 | 排序可演进 | 候选集、版本化分数、实验 | 🔶 Ranker+Hot 已完成，Hybrid 待做 |

## 4. M0：工程骨架

### 问题

新成员无法启动项目，配置散落在代码中，接口错误格式不一致，测试依赖本机数据库。

### 方案

- Hertz 提供 HTTP 路由和中间件。
- 配置从环境变量读取，开发环境用 `.env.example`。
- PostgreSQL 使用 [golang-migrate](https://github.com/golang-migrate/migrate) 管理 schema，运行时访问用 GORM。
    - 迁移文件路径：`migrations/000001_create_users.up.sql`
    - 选型理由与 GORM/migration 分工见补充文档 §8.4。`/health/ready` 依赖数据库检查见 §7 连接池配置。
- Docker Compose 启动 PostgreSQL、Redis、MinIO。
- 统一错误结构：`code`、`message`、`request_id`、`details`。完整的错误码表（12 个码）见补充文档 §5。
- 使用 `log/slog` 或 Zap 输出结构化日志。日志字段标准和脱敏规则见补充文档 §8。
- 认证使用 **JWT Bearer Token**（V1），详见补充文档 §1。V2 重构为 Session + Redis。

### 任务

- `GET /health/live`：进程存活。
- `GET /health/ready`：数据库和依赖可用。
- 请求 ID 中间件（UUID v7）。
- 统一 panic recovery。
- JWT 认证中间件（解析 `Authorization: Bearer <token>`，注入 `viewer_id` 到 context）。
- OpenAPI 文档。
- Testcontainers 集成测试。
- 数据库连接池配置初始值见补充文档 §7。

### 验证

- 删除本地依赖后，只运行一条 Compose 命令即可启动。
- 停止数据库时，readiness 返回失败，接口日志包含 request ID。
- CI 执行 lint、单元测试和集成测试。

### Mentor 检查点

- 为什么 liveness 和 readiness 要分开？
- 为什么配置不能直接写在全局变量里？
- Handler 和 Service 的边界在哪里？

> 本手册聚焦 Feed 系统的工程里程碑。认证方案（JWT V1 → Session V2）、完整 API 路由、统一错误码、缓存 Key 规范、MinIO 上传时序，以及 5 个关键 ADR，请一并阅读 [`feed-system-supplement.md`](./feed-system-supplement.md)。

## 5. M1：视频发布与内容状态

### 问题：视频文件和业务记录如何管理

视频文件很大，不适合放 PostgreSQL；上传过程中可能中断；未审核内容不能出现在 Feed。

### 方案

- MinIO 保存视频和封面，数据库只保存 object key、大小、时长和状态。
- 使用预签名 URL 让客户端直传对象存储。
- 视频状态机：

```text
UPLOADING -> PROCESSING -> PENDING_REVIEW -> PUBLISHED
      |          |              |
      +--------> FAILED <-------+
                       |
                    BLOCKED/DELETED
```

> **V1 简化**：项目不做真实转码，`PROCESSING` 和 `PENDING_REVIEW` 在 V1 实现中由创建接口自动瞬间跳过（创建后直接落到 `PENDING_REVIEW`，发布接口再置为 `PUBLISHED`）。状态机保留完整路径，未来插入真实审核流程时无需改表结构。完整时序见补充文档 §4。

- 只有 `PUBLISHED` 且 `published_at <= now()` 的视频允许进入 Feed。
- 定时任务清理超过一段时间仍处于 `UPLOADING` 的孤儿对象。

### 最小表结构

```sql
CREATE TABLE videos (
  id BIGSERIAL PRIMARY KEY,
  author_id BIGINT NOT NULL REFERENCES users(id),
  title TEXT NOT NULL,
  object_key TEXT NOT NULL,
  cover_key TEXT,
  duration_seconds INT NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  published_at TIMESTAMPTZ,
  like_count INT NOT NULL DEFAULT 0,
  comment_count INT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_videos_feed
  ON videos (published_at DESC, id DESC)
  WHERE status = 'PUBLISHED';

CREATE INDEX idx_videos_author
  ON videos (author_id, published_at DESC)
  WHERE status = 'PUBLISHED';
```

> `users` 表 DDL、`duration_seconds` 来源、计数冗余策略和所有外键关系见补充文档 §3。

### API

基础路径 `/api/v1`。完整 API 路由表见补充文档 §2。

```text
POST   /videos/upload-url   获取预签名上传 URL
POST   /videos              创建视频记录
POST   /videos/{id}/publish 发布视频（审核通过）
GET    /videos/{id}         视频详情（含 product_ids）
DELETE /videos/{id}         删除视频
```

> MinIO 预签名上传完整时序（upload-url → 客户端直传 → 创建记录）见补充文档 §4。

### 问题卡

| 问题 | 技术方案 |
|---|---|
| 上传重复 | 客户端幂等 key；object key 加 upload ID |
| 上传成功但数据库失败 | 状态记录 + 定时扫描孤儿对象 |
| 删除后仍可访问 | 查询过滤状态；删除事件清理缓存/索引；对象延迟删除 |
| 审核延迟 | 默认不可见，审核成功才改为 `PUBLISHED` |
| 用户访问他人私密视频 | 统一 `VisibilityPolicy`，不要在某个 Handler 单独判断 |

### 验证

- 发布中的视频不会出现在 Feed。
- 删除视频后，详情接口和 Feed 都不可见。
- 重复调用发布接口不会产生多次状态副作用。
- 中断上传后运行清理任务可以找到孤儿对象。

## 6. M2：时间排序 Feed 与游标分页

### 问题：为什么不能用 OFFSET 分页

```sql
SELECT * FROM videos
WHERE status = 'PUBLISHED'
ORDER BY published_at DESC
LIMIT 20 OFFSET 100000;
```

页数越深，数据库越需要扫描和丢弃大量记录；同时新视频插入会导致重复或漏数据。

### 方案：复合游标

排序键使用 `(published_at, id)`，两者一起保证稳定顺序：

第一页：

```sql
SELECT ...
FROM videos
WHERE status = 'PUBLISHED'
ORDER BY published_at DESC, id DESC
LIMIT 21;
```

下一页：

```sql
SELECT ...
FROM videos
WHERE status = 'PUBLISHED'
  AND (published_at, id) < ($1, $2)
ORDER BY published_at DESC, id DESC
LIMIT 21;
```

多查一条判断 `has_more`，只返回 20 条。

### 游标设计

游标内容：`published_at`、`video_id`、排序版本和用户场景。序列化后 Base64，并使用 HMAC 防止客户端篡改。

```json
{
  "published_at": "2026-07-30T08:00:00Z",
  "video_id": 123,
  "rank_version": "time-v1"
}
```

不要把数据库内部 offset 暴露给客户端。

### 问题卡

| 问题 | 技术方案 |
|---|---|
| 深分页慢 | 游标 + `(published_at, id)` 索引 |
| 时间相同 | 追加自增 ID 作为第二排序键 |
| 翻页重复 | 游标表示上一页最后一条，而不是当前时间 |
| 翻页漏数据 | 明确采用“向后翻页不回看新内容”的一致性语义 |
| 删除导致页大小不足 | 查询多取若干条并过滤；不要修改游标语义 |
| 游标被篡改 | HMAC 签名；解析失败返回 400 |
| 排序规则未来变化 | 游标携带 `rank_version`，版本不匹配时重新开始 |

### API 响应

```json
{
  "items": [
    {"id": 123, "author_id": 9, "title": "...", "cover_url": "..."}
  ],
  "next_cursor": "signed-base64",
  "has_more": true
}
```

### 验证

- 生成 100 万条视频，对比 OFFSET 和游标的 P95/P99。
- 请求第一页后插入新视频，再请求下一页，检查没有重复。
- 删除上一页或下一页的视频，检查游标仍可推进。
- 使用伪造 cursor 请求，必须返回明确错误。

## 7. M3：关注、点赞和互动幂等

### 问题：重复点击和并发操作会产生错误状态

客户端可能重复发送点赞、取消点赞或关注请求。单纯 `INSERT` 会触发唯一键错误，简单计数器也可能与关系表不一致。

### 方案

关系表使用唯一约束：

```sql
CREATE TABLE follows (
    follower_id BIGINT NOT NULL REFERENCES users(id),
    followee_id BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (follower_id, followee_id),
    CONSTRAINT ck_follow_no_self CHECK (follower_id <> followee_id)
);

CREATE INDEX idx_follows_followee
    ON follows (followee_id, follower_id);

CREATE TABLE likes (
  user_id BIGINT NOT NULL REFERENCES users(id),
  video_id BIGINT NOT NULL REFERENCES videos(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, video_id)
);

CREATE INDEX idx_likes_video ON likes (video_id, user_id);
```

> 关注上限 1000 人在服务层实施，见补充文档 §10 伪代码。

使用幂等语义：

- `PUT /videos/{id}/like`：重复调用结果仍是已点赞。
- `DELETE /videos/{id}/like`：重复调用结果仍是未点赞。
- 计数通过异步事件聚合，或由定时任务与关系表对账。

不要把 `like_count = like_count + 1` 当作唯一事实来源。

### 问题卡

| 问题 | 技术方案 |
|---|---|
| 重复点赞 | 唯一约束 + `ON CONFLICT DO NOTHING` |
| 并发取消/点赞 | 关系表状态为准，写入使用事务或明确最后写入语义 |
| 计数不准 | 原始事件幂等；异步聚合；定期对账 |
| 未登录刷接口 | Hertz 鉴权中间件 + 用户级限流 |
| 互动接口拖慢 Feed | Feed 不同步查询所有互动明细；批量查询当前用户状态 |

## 8. M4：Redis 缓存与读性能

### 问题：热点 Feed 反复查询数据库

当大量用户请求相同的热门 Feed，数据库会重复执行相同查询。缓存可以降低数据库压力，但引入了过期、不一致和热点故障。

### 方案选择

第一步只缓存“热门公共时间线的第一页”，不要一开始缓存所有用户的所有游标页：

```text
feed:global:v1:first-page -> video ID list
video:brief:{id}           -> 视频摘要
```

视频详情用 cache-aside：

1. 读 Redis。
2. miss 时通过 singleflight 合并请求。
3. 查 PostgreSQL。
4. 写入 Redis，TTL 加随机抖动。

### 问题卡

| 问题 | 技术方案 |
|---|---|
| 缓存击穿 | singleflight、逻辑过期、热点预热 |
| 缓存雪崩 | TTL 随机抖动、分批过期、数据库限流 |
| 缓存穿透 | 对不存在 ID 缓存短 TTL 的空值；入口参数校验 |
| 数据不一致 | 先写数据库再删缓存；接受短暂最终一致性 |
| Redis 故障 | 降级数据库、限制并发、熔断；不能无限重试 |
| 热点 key | 热门内容预计算、key 拆分或本地只读缓存 |
| 缓存过大 | 只缓存摘要和 ID；限制单 key 大小；监控内存淘汰 |

### 验证

- 指标：cache hit ratio、Redis P99、DB QPS、Feed P99。
- 使用 k6 同时请求热点视频详情，比较有无缓存时数据库 QPS。
- 停 Redis，确认接口仍能降级而不是请求风暴。
- 手动发布和删除视频，确认缓存最终刷新。

## 9. M5：播放事件异步化

### 问题：同步记录播放会拖慢 Feed

播放频率远高于发布频率。如果每次播放都同步写数据库，数据库写入会成为 Feed 读请求的尾延迟来源。

### 方案

API 快速接收事件并发送 Kafka：

```text
POST /videos/{id}/play
  -> 校验轻量字段
  -> 写入 Kafka
  -> 立即返回 accepted

Kafka -> play-worker -> 原始事件表/聚合表
```

### play_events 表结构

```sql
CREATE TABLE play_events (
    event_id UUID PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    video_id BIGINT NOT NULL REFERENCES videos(id),
    session_id VARCHAR(128),
    played_at TIMESTAMPTZ NOT NULL,
    trace_id VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

> V1 中 play_events 通过 Kafka consumer 写入，`event_id` 由客户端生成（UUID v7），用于幂等去重。完整的索引策略和说明见补充文档 §3.5。

事件字段：`event_id`、`user_id`、`video_id`、`occurred_at`、`session_id`、`trace_id`。

消费者至少一次消费，因此使用 `event_id` 唯一约束或幂等表。计数聚合允许延迟，但不能因重复消息无限增长。

### 问题卡

| 问题 | 技术方案 |
|---|---|
| 重复事件 | event ID 幂等 |
| 消息发送失败 | 本地 Outbox 或返回可重试错误；不要假装成功 |
| 消费者崩溃 | Kafka offset 在处理成功后提交 |
| 消息积压 | 监控 consumer lag，扩容 Worker，设置过期事件策略 |
| 乱序事件 | 按 video ID 分区或聚合时使用事件时间窗口 |
| 统计口径变化 | 记录事件 schema/version，不覆盖原始事件 |

### 验证

- 重放同一批事件，聚合结果不重复。
- 停止 Worker，观察 lag；恢复后能追平。
- Feed 请求不因 Worker 停止而明显变慢。

## 10. M6：关注 Feed 的两种实现

### 问题：用户只想看关注作者，但关注数增长后查询变慢

**核心参数**：每位作者取 K=5 条最新视频，关注上限 1000 人。完整查询伪代码见补充文档 §10。

设用户关注了 N 个作者，每次请求都查询 N 个作者的最新视频，再合并排序，就是拉模式；把新视频写入每个粉丝的 Feed，则是推模式。

### 拉模式：先实现它

```text
读取关注列表
  -> 批量查询作者最近 K 条视频
  -> 内存归并排序
  -> 游标截断
```

适合第一版，因为实现简单、写入成本低，适合关注关系较少的用户。

优化：批量查询、限制作者数量、缓存关注列表、只拉候选窗口，不读取所有历史内容。

### 推模式：作为演进实验

发布视频时写入粉丝收件箱：

```text
VideoPublished -> Kafka -> fanout-worker -> feed:inbox:{user_id}
```

优点是读取快，缺点是写放大严重。大 V 发布一个视频可能需要写入大量用户。

### 混合策略

- 普通作者：推给粉丝。
- 超大作者：不推，读取时拉取。
- Feed 请求时合并 inbox 和大 V 内容。

### 验证

构造 10、1,000、100,000 粉丝的作者，比较发布延迟、写入量和读取 P99，形成一页实验报告。不要直接声称某种模式永远更好。

## 11. M7：可见性、审核和删除

### 问题：内容在多个缓存和异步系统中传播

视频发布后可能被审核封禁、作者删除、用户拉黑或设置为私密。如果只删除主表，缓存、搜索和 Feed 收件箱中仍可能存在旧内容。

### 方案

统一可见性判断：

```go
type VisibilityPolicy interface {
    CanView(ctx context.Context, viewerID, videoID int64) (bool, error)
}
```

防线分三层：

1. 写入 Feed 候选时只接受可发布状态。
2. 读取 Feed 时再次过滤状态和观众权限。
3. 状态变化发布 `VideoBlocked`/`VideoDeleted` 事件，清理缓存、收件箱和搜索索引。

读取时的二次过滤是必要的，因为异步清理必然存在延迟。

### 验证

- 发布后立即封禁，连续请求 Feed、详情、搜索三个入口。
- 删除视频后重放清理事件，重复消费不报错。
- 私密视频不能被非授权用户访问。

## 12. M8：排序演进，但不急着做推荐模型

### 问题：时间排序无法体现内容质量

先定义排序接口，不要把排序逻辑写死在 SQL Handler 中：

```go
type Ranker interface {
    Rank(ctx context.Context, candidates []Candidate, scene Scene) ([]Candidate, error)
}
```

演进：

1. `TimeRanker`：发布时间。
2. `HotRanker`：点赞、播放完成率、发布时间衰减。
3. `HybridRanker`：关注、热门、新内容按比例混排。

每个排序结果记录 `rank_version`，便于回放和比较。候选召回和排序分离：先找可能的内容，再计算顺序。

### 冷启动问题

新用户没有行为时使用热门、新内容和类别兜底；新作者没有互动时给予有限探索流量。冷启动规则必须可配置并有指标，不要写死在多个分支中。

### 验证

- 固定一批候选，比较不同 ranker 的结果。
- 记录点击率、完播率和负反馈率；不要只看点击率。
- 用固定 `rank_version` 重放同一请求，结果应可解释。

## 13. 观测与故障排查

### 必备指标

HTTP：QPS、错误率、P50/P95/P99、按路由和状态码拆分。

Feed：空 Feed 比例、重复率、游标错误率、缓存命中率、内容过滤数量。

Redis：命中率、命令 P99、连接池耗尽、内存使用、淘汰数。

Kafka：生产失败、消费失败、consumer lag、重试次数、死信数量。

业务：播放事件接收量、落库延迟、点赞成功率、视频发布到可见的耗时。

### 一次请求要能追踪

```text
request_id/trace_id
  -> Hertz handler
  -> feed service
  -> PostgreSQL/Redis
  -> Kafka（如有）
```

日志必须包含 `user_id`、`video_id`、`cursor_version`、`cache_hit` 和耗时，但要脱敏，不记录 Token 或隐私内容。

### 故障演练清单

- PostgreSQL 延迟升高：降低并发、返回降级 Feed、告警。
- Redis 完全不可用：走数据库并限制请求，不能无限重试。
- Kafka 不可用：播放事件进入本地 Outbox 或明确返回可重试状态。
- Worker 停止：Feed 仍可读，事件 lag 可见，恢复后能追平。
- 缓存填充失败：singleflight 结束后返回数据库结果，不阻塞所有请求。

每次演练记录：故障注入方式、用户表现、指标变化、恢复步骤和后续改进。

> 更详细的缓存 Key 命名规范、数据库连接池配置和结构化日志字段标准见补充文档 §6-§8。

## 14. 测试与压测计划

### 单元测试

- 游标编码/解码和签名。
- 时间排序和去重。
- 可见性策略。
- 点赞幂等。
- Ranker 的分数和 tie-break 规则。

### 集成测试

- PostgreSQL 真实分页和索引。
- Redis cache-aside 和降级。
- Kafka 事件消费幂等。
- MinIO 上传状态。

### 端到端测试

```text
创建视频 -> 发布 -> Feed 可见 -> 点赞 -> 播放事件 -> 聚合结果
```

### k6 场景

1. 90% 读 Feed，10% 读视频详情。
2. 热点视频详情突发请求。
3. 大量新视频发布与 Feed 并发读取。
4. Redis 故障期间的降级请求。
5. 关注数分别为 10、1,000、100,000 的 Feed 请求。

压测报告至少包含环境、数据量、并发、QPS、P95/P99、错误率、数据库 QPS、缓存命中率和结论。

## 15. 你每周应该交付什么

### Week 1

工程骨架、视频表、健康检查和迁移。

### Week 2

MinIO 上传、视频状态机、视频详情和删除。

### Week 3

时间排序 Feed、游标、复合索引和分页测试。

### Week 4

点赞、关注、幂等和可见性策略。

### Week 5

Redis 视频摘要缓存、singleflight、指标和故障测试。

### Week 6

播放事件、Kafka、Worker、消费幂等和 lag 监控。

### Week 7

关注 Feed 拉模式、热点实验和压测。

### Week 8

简单热门排序、故障演练、架构复盘和面试材料。

每周结束必须有：一个可演示功能、一份测试结果、一条提交记录和一段复盘。没有完成的功能缩小范围，不把债务无限推到最后。

## 16. 面试复盘模板

对每一个技术点准备四句话：

1. 当时遇到什么可测量的问题？
2. 为什么选择这个方案而不是另一个方案？
3. 失败时系统怎么表现，如何恢复？
4. 用什么测试或指标证明方案有效？

例如：

> Feed 深分页导致 P99 上升，所以使用 `(published_at, id)` 游标和复合索引。代价是客户端不能随机跳页，因此产品只支持向后翻页。通过百万数据压测，P99 从 OFFSET 的高位稳定到游标方案的低位，并补充了并发插入、删除和伪造游标测试。

这类回答比“用了 Redis 和 Kafka”更能体现工程能力。
