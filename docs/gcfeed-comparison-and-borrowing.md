# GCFeed 对比与借鉴报告

> 对比对象：[LeoninCS/GCFeed](https://github.com/LeoninCS/GCFeed) —— 短视频 Feed 系统
> （Go API 单体 + React Web + MySQL + Redis + RabbitMQ，约 1.8 万行 Go / 114 个文件，
> checkout `8cf995c`）。**它与 TokMall 的核心域（短视频 Feed + 社交）高度重叠**，
> 是三个对照项目里最贴近的一个。
>
> 前两份报告：[DomTok 对比报告](domtok-comparison-and-borrowing.md)（电商微服务）、
> 其中附录含 `fzuhelper-server` 的核查结论。
>
> 本文回答：**GCFeed 里有什么值得搬到 TokMall 当亮点，有什么必须绕开。**
> 已落地的两项在 §3 给出代码位置与实测证据。

---

## 1. 一句话结论

> **GCFeed 的产品面比 TokMall 宽（推荐、曝光、消息中心、播放 QoS、热榜），
> 但可靠性工程面系统性弱于 TokMall 现状；真正值得抄的是四件事的"结构"而不是"算法"。**

它给出的最有价值的东西是一个**卡点解法**：TokMall 的 `HotRanker` 一直因
"排序键未与游标闭合"被 fail-fast 禁止启用，而 GCFeed 演示了那条缺失的思路——
**把易变的排序键物化成"按窗口命名的不可变快照"，让快照 ID 成为游标的一部分**。

---

## 2. 两个项目的定位差异

| 维度 | GCFeed | TokMall |
|---|---|---|
| 形态 | Go API 单体（Gin）+ React，另有一个 worker 进程 | Hertz 模块化单体 + React |
| 数据 | MySQL + GORM **AutoMigrate** | PostgreSQL + golang-migrate 版本化迁移（000001~000020） |
| 消息 | RabbitMQ（3 exchange / 4 queue，无 DLQ、无 prefetch） | Transactional Outbox + Redis Streams（幂等消费 + DLQ + 人工重放） |
| 缓存 | Redis（页缓存/卡片/计数/热榜/inbox，**无主动失效，只等 TTL**） | Redis + 进程内 L1（版本号跨实例失效）+ 布隆过滤器 + 负缓存 + 慢命令 Hook |
| 一致性 | "MySQL 是事实，Redis 等 TTL"，**无对账** | 9 项对账检查 + 对账驱动重放 + 读模型重建 |
| 分页 | base64(JSON) **无签名**游标（时间线 (published_at,id)、热榜 window_end+offset） | HMAC 签名游标（时间流 + 新增热榜分数游标） |
| 观测 | 12 个指标 + 7 面板，**零告警规则** | 应用指标 + 8 记录规则 + 7 告警 + 11 面板 + 规则引用校验 |
| 测试 | 46 个用例**全部是内存 fake + httptest**，无真实依赖；无并发测试 | 真实 PG/Redis 隔离 schema 集成测试 + 并发证明 + -race + e2e |
| CI | **无**（无 .github、无 Makefile、无 lint） | 4 job + CodeQL + govulncheck + golangci-lint（精选 14 个） |
| 推荐 | 有完整链路（单路召回 + 向量排序 + 曝光去重），但向量是**占位实现** | 无推荐域 |
| 播放 | 有配置下发 + 预加载 + QoS 打点 + **上传期媒体校验** | 只有 play_events 打点，**上传零校验** |

结论：**产品能力它更宽，工程正确性 TokMall 明显更强。** 借鉴时必须"取结构、弃实现"。

---

## 3. 已落地的两项（含实测证据）

### 3.1 热榜：分钟桶 + 冻结窗口 + 分数游标（ADR-027）

**它怎么做**：点赞/收藏/评论按分钟写进 ZSET（`feed:hot:minute:{yyyyMMddHHmm}`，TTL 2h），
读取时 `ZUNIONSTORE` 合并最近 60 个桶成窗口，`ZREMRANGEBYSCORE -inf 0` 清掉净分 ≤ 0 的
条目，窗口带短 TTL 缓存；游标里带 `window_end`。

**为什么值得**：这正好补上 TokMall 的缺口——`HotRanker` 的分数依赖请求时刻，
所以 `SupportsCursorPagination()` 只能是 false。把分数**物化**成"按窗口命名的快照"后，
排序键就是冻结的、可游标的。

**TokMall 的实现**（`internal/feed/hot.go`、`hot_cache.go`、`hot_service.go`）**修掉了它的三个缺陷**：

| 它的缺陷 | 证据 | TokMall 的做法 |
|---|---|---|
| 缓存路径用 `window_end + offset` 翻页，窗口重建后 offset 漂移 → 重复/漏数 | `application/feed/service.go:340-372` | **冻结窗口 + 分数 keyset 游标**：游标带 `(window_end, hot_score, video_id)`；同分用 video_id 次级排序。有用例专门验证"翻页途中删窗口重建 + 改分数，第二页不重复" |
| 窗口重建**没有 singleflight**，且窗口内无正分成员时 `ZUNIONSTORE` 不创建 key → 每个并发请求都重做 60-key 合并（自造击穿） | `infra/cache/feed_cache.go:385-411` | 三层防护：**后台 worker 每分钟物化** + **singleflight 合并回源**（不继承首个请求 ctx）+ **double-check** |
| Redis 故障时热榜直接 500（DB 兜底路径只在 `cache == nil` 时启用，走不到） | `application/feed/service.go:474-481` | **降级为时间流**并打点 `hot_feed_degraded_total{reason}`，响应结构不变 |

另外两处改进：
- **幂等计分**：它按客户端请求计分（评论只在客户端主动传幂等键时去重，不传就能无限刷，
  热度榜可被单人刷爆）。TokMall 让仓储返回"是否真的插入/删除行"，只有**状态真的变化**
  才计分——重复点赞两次，`hot_score` 仍是 3（实测）。
- **窗口硬上限**：`ZREMRANGEBYRANK` 裁到 2000 条（它只对 inbox/outbox 做了容量上限）。

**实测**：

```
POST /videos/{id}/like ×2（同一用户，幂等）+ 另一视频 ×1 + 播放 ×1
GET /api/v1/feed/hot?limit=3
  video=87 hot_score=3    ← 点赞两次仍只计 3 分
  video=86 hot_score=3
  video=85 hot_score=1    ← 播放 +1
分页：page1=[(87,3),(86,3)] has_more=true → page2=[(85,1)]（同分正确跨页）
伪造游标 → HTTP 400（HMAC 签名校验）
指标：hot_feed_score_updates_total{action="like"}=2、{action="play"}=1、
      hot_feed_window_rebuild_total{source="scheduled"}=1、{source="on_demand"}=1、
      hot_feed_window_entries=3、hot_feed_source_total{source="window"}=1
```

### 3.2 上传媒体校验：ffprobe + faststart remux（ADR-028）

**它怎么做**：上传 handler 里用 ffprobe 校验时长/分辨率/编码白名单，
再用 ffmpeg `-c copy -movflags +faststart` 重写文件把 moov 前置。

**为什么值得**：TokMall 的上传是预签名 PUT 直传，`Create` 只做 `StatObject`（存在且非空）
+ 客户端声明的时长范围校验——**服务端对"这是不是一个能播的视频"一无所知**；
而且 moov 在尾部的 mp4 必须被完整下载才能出首帧，预签名 GET 直连对象存储时
这个代价直接落在用户身上。

**TokMall 的实现**（`internal/mediaprobe` + `internal/video/probe_worker.go`）
**改掉了它的实现方式**：它在上传请求里**同步**跑 ffprobe(30s 超时) + ffmpeg(2min 超时)，
阻塞 HTTP 线程、依赖宿主机装了 ffmpeg、失败直接 500。TokMall 放到**异步 worker**：

```
PENDING 取件（部分索引）→ 下载到临时目录（处理完必删）→ ffprobe 实测
→ 策略校验 → 需要时 remux 并回写 → 条件更新写回结果（WHERE status='PENDING'）
```

三条设计原则（都写进了代码注释）：
1. **不阻塞用户请求**：校验慢只影响"什么时候可发布"；
2. **工具缺失 ≠ 文件损坏**：`ErrToolMissing` → 标记 SKIPPED 并告警（降级放行），
   `ErrProbeFailed` → REJECTED。绝不能因为宿主机没装 ffprobe 就把正常视频判成坏文件；
3. **实测值与客户端声明分开落库**：`probed_duration_seconds` vs `duration_seconds`，
   出问题时能对比"用户说多长"与"实际多长"。

**实测**（真实 ffmpeg + 真实 MinIO + 真实 PG）：

| 场景 | 结果 |
|---|---|
| 上传 30s / 1.1MB / **moov 在尾部**的 mp4 | `PASSED`，`probed_video_codec=h264`，`probed_duration=30.000`，`faststart_applied=true` |
| 直接读 MinIO 存储文件 | **`moov` 偏移 68 < `mdat` 偏移 3902**（faststart 真的落到了对象上） |
| 上传 1.2MB 随机数据（声明 5s） | `REJECTED`，原因 "not a playable video: media probe failed" |
| 指标 | `media_probe_result_total{status="passed"}=1`、`{status="rejected"}=1`、`media_faststart_applied_total=1` |

单测（真实素材）：探测真实文件、策略逐条拒绝、**moov 尾部 → remux 后前置**、
非媒体文件报 `ErrProbeFailed`、缺工具报 `ErrToolMissing`。

---

## 4. 值得借鉴但本轮未落地的（含设计要点）

| # | 借鉴点 | 它怎么做 | 面试价值 | 成本 | 为什么要改着抄 |
|---|---|---|---|---|---|
| 1 | **曝光去重**（双表：流水 + 聚合） | `video_view_events`（append-only 流水）+ `exposures`（`UNIQUE(user_id,video_id)` + `last_exposed_at`），召回时 `LEFT JOIN ... AND last_exposed_at >= now-7d` 反连接过滤；决策接口返回显式 `allowed/reason` | ⭐⭐⭐⭐ "Feed 不重复"是产品刚需；`allowed/reason` 的 API 形状比默默丢弃干净 | 中 | **不要抄它的查询**：反连接的成本随曝光表线性上升，且推荐链路硬依赖该表。应改成 Redis 每用户 ZSET（score = 曝光时间戳，`ZREMRANGEBYSCORE` 清 7 天前），DB 只做落库与审计 |
| 2 | **推拉结合的关注流** | 小 V 发布时把视频写进粉丝 inbox ZSET（上限 1000）；粉丝数 ≥ 10000 的大 V 只写自己的 outbox（上限 500），读时把 inbox + 关注的大 V outbox 合并、按 `(published_at,id)` 游标裁剪 | ⭐⭐⭐⭐⭐ "推拉结合 + 大 V 阈值"是 Feed 系统的标准面试题；TokMall 现在是纯拉模式（AGENTS.md 明确记为待演进） | 大 | 它的实现有四个问题：**取关不清理 inbox**（取关后历史推文仍在关注流里，直到容量裁剪或 30 天 TTL）、每页对每个大 V outbox 发一次命令（关注 500 个大 V = 1001 次命令/页）、`hasIndex` 判定过宽导致 DB 兜底失效、Redis 故障直接 500。要抄的是"推拉阈值 + inbox 容量上限"这两个结构，实现必须重写 |
| 3 | **端侧播放配置 + QoS 打点** | `playback_config(platform, network_type)` 下发 preload_count/buffer_ms（带 `Web/default` 兜底）、`/preload-videos` 按游标返回后续资源、`playback_qos_log`（首帧/卡顿/时长，`(user_id,idempotency_key)` 唯一索引幂等） | ⭐⭐⭐⭐ "首帧/卡顿"是视频产品唯一真实的体验指标，TokMall 完全没有 | 小 | 无冲突，纯新增；注意它的 view event **没有幂等**（可无限刷、表无限增长），TokMall 要用已有的 `event_id` 幂等模式 |
| 4 | **分片计数器**（热 key 写扩散） | 单个视频的点赞计数按 `userID % 16` 拆到 16 个 Redis Hash 分片，读时求和（base + Σshards） | ⭐⭐⭐ "热点 key 怎么打散"是缓存面试必问 | 中 | **只抄分片，不抄它的 base 设计**：它的 base 用 `HSETNX` 只在不存在时写、TTL 24h 又不断续期，导致热点视频 base 永不刷新、分片过期后计数**只减不增**（永久偏低），且全仓无对账无刷盘。TokMall 要用它必须先有"Redis 计数 → DB 定期刷盘 + 对账校验"的闭环 |
| 5 | **向量/embedding 的 schema 形状** | `video_embedding` 用 `(video_id, model)` 复合唯一 + `text_hash` 记录内容版本 + 重复发布事件 upsert 覆盖 | ⭐⭐⭐ "向量与模型版本一起落库"是干净写法 | 小 | **只抄 schema，不抄算法**：它是 128 维本地 hashing n-gram（FNV-1a + 符号哈希）对 `title+description` 做暴力余弦，无外部模型、无 ANN、向量以 JSON 存 MySQL 全量捞出。它自称的"用户兴趣向量 worker"**根本没有消费者**（`ConsumeViewEventRecorded` 全仓无调用点，队列只进不出会无限堆积） |

---

## 5. 必须绕开的坑（带证据）

| # | 坑 | 证据 | 说明 |
|---|---|---|---|
| 1 | **有生产者无消费者的队列** | `infra/mq/rabbitmq.go:241` 定义了 `ConsumeViewEventRecorded`，全仓无调用点；`router.go:107` 却在发布 | 每次看视频都往 durable 队列堆一条，无 TTL、无 max-length、无 DLQ → 无限增长。**"声明了拓扑"不等于"链路通了"** |
| 2 | **发布无 confirm 且丢弃错误** | `video/service.go:92`、`exposure/service.go:73` 的 `_ = s.publisher.Publish...` | 业务事件静默丢失。TokMall 的 Outbox 是"业务事实与事件同事务" |
| 3 | **无 DLQ / 无重试上限 / 无 prefetch** | `rabbitmq.go:187` 失败一律 `Nack(false, true)` 立即重入队；`QueueDeclare` 无 `x-dead-letter-*`；全仓无 `.Qos(` | 毒消息无限热循环 + 无背压。TokMall 有 DLQ + 人工重放 |
| 4 | **Redis 计数永久偏低** | base `HSETNX` 不刷新（`feed_cache.go:604-612`）+ 分片 24h TTL（`:497`） | 读路径 `base + Σshards` 只减不增，且无对账修复 |
| 5 | **"先写 Redis 再发消息"无原子性** | `interaction/service.go:310-319`：Redis 状态先改，publish 失败才降级同步写 | 进程在两者之间崩溃 / 请求 ctx 取消 → 幽灵点赞（Redis 已计、DB 没有），且 `actionStateTTL=30天` 让不一致挂 30 天 |
| 6 | **锁序不一致导致死锁** | `relation/gorm.go:48-53` 先锁 `userID` 再锁 `targetUserID`，`updateStat` 同序 | A 关注 B 与 B 关注 A 并发 → 锁序相反 → MySQL 1213，用户拿 500 |
| 7 | **状态机退化为无条件更新** | `video/gorm.go:184-196` 用 `WHERE id = ?`，没有 `WHERE status = from` | 与 TokMall 的条件更新状态机相比是退步（重复删除靠 service 短路，仍有 TOCTOU 窗口） |
| 8 | **AutoMigrate + 启动期裸 DDL** | `migration/migration.go:20-58`（14 个 model，API 与 worker 各跑一次，靠 1060/1061 重试糊并发 DDL）；`feed/gorm.go:31-45` 运行时 `CREATE INDEX` | 无版本、无回滚、无评审。TokMall 用 golang-migrate（000001~000020） |
| 9 | **游标无签名** | `feed/service.go:837-869` 只是 `base64(JSON)`；`parseHotCursor` 甚至接受客户端传来的 `RankScore`/`Offset` | 可伪造跳页/遍历。TokMall 的 HMAC 签名游标更强 |
| 10 | **推荐游标不自洽** | `recommendation/service.go:122-135` 先用末条分数生成游标，**之后**才 `interleaveByAuthor` 重排返回页 | 返回顺序 ≠ 游标顺序 → 下一页重复；且 `RankScore` 每请求现算（freshness 依赖当前时间），分数漂移即漏条 |
| 11 | **单路召回、无探索** | 唯一召回是一条 `video LEFT JOIN video_stat LEFT JOIN exposures ... ORDER BY (like*3+comment*5+fav*4) DESC LIMIT 50~500` | 不在热度 TOP-N 的新视频永远召不回；用户 7 天看完 TOP-500 后推荐流返回空 |
| 12 | **测试全部是内存 fake** | `apps/api/test/` 46 个用例，grep `gorm.Open|redis.NewClient|testcontainers` 零命中 | 唯一约束冲突、`ON CONFLICT`、`FOR UPDATE` 锁序、Redis `Watch`、TTL 漂移、MQ `Nack/requeue` **一个都没覆盖**。`infra/cache/feed_cache_test.go` 的 fake 连 `Watch` 都不实现，等于绕开了核心路径 |
| 13 | **性能数字是文档示例而非实测** | `docs/performance-testing.md:236-255` 的 "19.85 QPS / P95 17.96ms" 正好等于脚本参数（20 VU × 1 req/s × 60s）的算术上限；全仓零 `func Benchmark`、无结果文件、git 只有一个 squashed commit | 而 `docs/optimization.md` 通篇没有一个数字，验收标准全是"稳定在可接受范围"这类定性描述 |
| 14 | **零告警规则** | `apps/monitoring/grafana/provisioning/alerting/` 只有一个 `.gitkeep`；`prometheus.yml` 无 `rule_files`；无 alertmanager | 只有"看"没有"报"。TokMall 有 8 记录规则 + 7 告警 + 规则引用校验测试 |
| 15 | **无日志/无 trace** | 非测试代码只有 2 处 `log.Printf` | 500 无栈无 ID。TokMall 有 slog ContextHandler（自动注入 request_id/trace_id/route）+ 脱敏 |
| 16 | 其他 | 假锁（事务外用 `FOR UPDATE`：`message/gorm.go:118-120`、`playback/gorm.go:130`）、单条 INSERT 套事务、`isDuplicateKeyError` 用字符串包含、配置默认值即密钥、`/metrics` 挂在业务端口无鉴权、Grafana admin/admin | 都是细节但成片出现 |

---

## 6. 一句话给面试

> 我读的第三个同类项目是一个短视频 Feed 系统，和我的项目核心域几乎重合。
> 我从它那里抄了两件事：**热榜的"分钟桶 + 冻结窗口"结构**和**上传后的真实媒体校验**。
> 但两件都不是照抄——它的热榜用 `window_end + offset` 翻页，窗口一重建 offset 就漂移，
> 我改成"冻结窗口 + 分数 keyset 游标"，并且发现它的窗口重建没有 singleflight、
> 窗口为空时每个请求都会重做一次 60-key 合并，于是补了后台物化 + singleflight + double-check；
> 它的媒体校验同步跑在上传请求里（ffprobe 30s + ffmpeg 2min 超时，失败 500），
> 我改成异步 worker，并且把"工具没装"和"文件坏了"区分开——
> 前者降级放行，后者才拒绝。
>
> 它的**产品面比我的宽**（推荐、曝光去重、消息中心、播放 QoS），
> 但**工程正确性比我的弱**：没有对账、没有 DLQ、没有签名游标、测试全是内存 fake、
> 性能数字是文档里的示例而不是实测、零告警规则。所以我的结论是：
> **抄它的结构，不抄它的实现；它踩过的坑（计数永久偏低、锁序死锁、
> 有生产者没消费者的队列）我都写进了避坑清单。**
