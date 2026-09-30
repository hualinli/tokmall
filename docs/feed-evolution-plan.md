# Feed 演进计划：对照 GCFeed

> 计划，未实现。核对基点：TokMall `main`（2026-09-26）与 [GCFeed `8cf995c`](https://github.com/LeoninCS/GCFeed/tree/8cf995cfa03985c335594c453ea050df5c0f1b6a)。这里只选能解决 TokMall 已知问题或可由实验验证的机制；不把 GCFeed 的设计当成已经验证的 TokMall 性能结论。当前接口以 [OpenAPI](../internal/api/openapi.yaml) 为准。

## 1. 现状与可借鉴点

TokMall 生产 Feed 目前是全局时间流与关注时间流，使用 `(published_at,id)` 签名游标、PostgreSQL 拉取、Redis 第一页缓存。关注流已从每作者 TOP 5 改为完整时间线：先查关注数，0 关注直接返回，少量关注作者使用每作者有界归并，多关注作者沿全局时间索引过滤；两种路径均返回 `limit+1` 条。旧压测仍对应旧查询。`HotRanker` 只用于包内实验，因为热度键与当前游标未闭合。点赞数每次请求批量查 `likes`，商品卡按页动态装配。详见 [当前实现](../internal/feed/service.go)、[查询](../internal/feed/repository.go)、[历史压测](performance-baseline.md)。

| GCFeed 的做法 | TokMall 目前的差距 | 决定 |
|---|---|---|
| [按 timeline/hot/following/recommend 场景组织服务](https://github.com/LeoninCS/GCFeed/blob/8cf995cfa03985c335594c453ea050df5c0f1b6a/apps/api/internal/application/feed/service.go) | 只有两条正式时间流；推荐需求与排序契约尚未定义 | 借鉴场景边界，先明确产品语义，再新增路由；不一次性复制所有场景 |
| 同一服务中将页 ID、视频卡片、统计分层缓存，批量装配 | TokMall 缓存第一页主体，点赞数/商品卡实时查；性能与新鲜度代价尚无混合负载数据 | 作为 F2 实验，按测量结果逐层引入 |
| [小作者写粉丝 inbox、大作者保留 outbox 的混合分发](https://github.com/LeoninCS/GCFeed/blob/8cf995cfa03985c335594c453ea050df5c0f1b6a/apps/api/internal/application/video/fanout_worker.go) | 关注流完全拉取，尚无高关注数读写成本曲线 | 仅在 F3 门槛出现时实施；必须先有可靠事件投递 |
| [点赞写 Redis 并异步经 MQ 落库](https://github.com/LeoninCS/GCFeed/blob/8cf995cfa03985c335594c453ea050df5c0f1b6a/apps/api/internal/application/interaction/service.go) | TokMall 同步写 PostgreSQL，唯一约束保证幂等 | 暂不移植。先测点赞写入与 Feed 读路径是否真受限，并设计失败回放、状态对账 |
| [场景压测与指标](https://github.com/LeoninCS/GCFeed/blob/8cf995cfa03985c335594c453ea050df5c0f1b6a/docs/performance-testing.md) | 现有基线主要是特定时点的局部实验 | 立即借鉴方法，建立可复现实验矩阵 |

GCFeed 的发布路径存在 [数据库成功后发布事件、忽略发布错误](https://github.com/LeoninCS/GCFeed/blob/8cf995cfa03985c335594c453ea050df5c0f1b6a/apps/api/internal/application/video/service.go) 的失效窗口。TokMall 若让 inbox 成为交付依据，需要事务 Outbox、可重试 relay 和幂等消费，不能直接照搬该段流程。

## 2. 执行顺序

### F0：先修语义与一致性边界

1. **关注流定义（已实现并验证）**：`/feed/following` 采用完整时间线。原每作者 TOP 5 会让单个高频作者的第 6 条以后消失；现在两种查询路径都按全局时间键取 `limit+1`，分页时不漏旧视频。真实 PostgreSQL 集成测试覆盖单作者超过 5 条、多作者交错、同时间戳、未发布/未关注过滤、0 关注、关注作者无作品、33 关注走全局路径和翻页期间新增视频。旧 M6 压测数字不能当作新查询的容量结论。
2. **缓存竞争**：构造“旧读已开始 → 发布/删除提交并删缓存 → 旧读回填”的确定性测试；给缓存引入版本号/代际键或等效条件写，保证旧读不能覆盖新代。覆盖全局和关注第一页、关注/取关、删除后的可见性。保留 TTL 兜底，但明示容忍窗口。
3. **游标契约**：排序键、游标内容与版本必须一致；新增场景前测翻页无重复/漏项、时间窗跨界、旧版本游标失效。热榜不能复用时间游标。

**完成证据**：API 语义写入 OpenAPI；真实 PG + Redis 竞争/分页测试通过；文档说明可见性和缓存新鲜度。F0 属正确性收口，优先于性能优化。

**第一项的执行计划记录（2026-09-26）**：[`scripts/explain_following.sql`](../scripts/explain_following.sql) 在 PostgreSQL 临时表造 1000 作者、10 万均匀分布视频，并复刻现有索引。`EXPLAIN (ANALYZE, BUFFERS)` 第一页：1000 关注全局索引约 0.07ms、每作者归并约 62ms；10 关注约 0.55ms / 0.63ms；10 关注但作者无作品约 0.23ms / 0.04ms。故先按关注数选择查询：0 直接返回，1～32 用每作者有界归并，更多用全局索引。阈值是实验起点，不是生产容量结论；脚本只测 SQL，不含额外的关注数查询、点赞数、URL 签名与网络延迟，F1 仍需完整数据集与混合负载压测。

### F1：建立可重复的 Feed 负载基线

固定代码提交、数据规模、机器/容器资源、连接池、缓存预热、测试时长和压测脚本。覆盖全局首页命中/失效、深分页、关注 0/10/100/1000 作者、单高频作者、点赞与发布混合写入；至少分别测冷缓存、热缓存和 5～10 分钟混合负载。记录吞吐、P50/P95/P99、错误率、DB 池等待与慢 SQL、Redis 命中/延迟、每场景查询次数、应用 CPU/内存。重复运行并保留原始结果；与 [旧基线](performance-baseline.md) 区分测试条件，不直接比较两个不同环境的 QPS。

**完成证据**：可运行脚本、种子数据、结果表和瓶颈归因。先给出可接受的场景 SLO，再决定 F2/F3 的必要性。

### F2：按瓶颈拆页、卡片和统计缓存

- 如果热页的反序列化/Redis 带宽成为瓶颈，实验只缓存 ID 页 + 批量卡片；对照现有完整主体缓存，比较总 QPS、P95、回源次数和内存，不预设必胜。
- 如果 `LikeCounts` 的聚合或商品卡批量查询成为瓶颈，分别考虑短 TTL 统计缓存和稳定卡片缓存。点赞、商品上下架、挂载/取消、删除各自定义可见性时限与失效责任；商品售卖状态必须在下单时重新验证。
- 任何新缓存都要测热点键、单实例/双实例、失效风暴、Redis 不可用及旧读回填。只保留实测有收益且一致性规则清楚的层。

**完成证据**：相同环境 A/B 数据、缓存命中与 DB 负载下降、变更后可见性测试，以及回滚开关。

### F3：热榜和混合收件箱，按需分步做

1. **热榜先做实验**：定义播放/点赞权重、反作弊口径、时间窗口、快照版本与确定性 tie-break；用固定时间窗的快照键做分页，测试更新期间无重复/漏项，再开放独立 hot 路由。GCFeed 的分钟桶可作候选，先与 TokMall 的 TimeRanker 对比延迟及可解释性。
2. **关注流仍慢时再做推拉混合**：用 F1 的关注数、发布频率和读写比计算阈值。普通作者发布后分批写 follower inbox；高粉作者在读取时从 outbox 合并。约定去重、容量、游标、删除过滤、TTL/重建、积压与降级读 PG。
3. **可靠性前置**：只有发布事件与视频事务同库落 Outbox、relay 可重试、consumer 幂等、重放/对账齐备后，inbox 才可承担读路径。实测全链路延迟、fanout 积压和故障恢复。该 Outbox 可与 [Commerce 阶段 4](interview-production-readiness-plan.md) 共享基础设施，但事件和消费边界独立。

**进入条件**：F1/F2 证明当前拉模式或时间流不能达到约定 SLO，且有明确热榜/推荐产品需求。没有触发条件时不引入 MQ fanout 或异步点赞。

## 3. 每阶段交付与停止规则

每阶段独立提交代码、测试、压测脚本/原始数据、前后对比和一页设计决策。先完成 F0、F1；F2 只做被数据指向的层；F3 需额外评审事件可靠性与运维成本。所有收益都写清测量环境和新鲜度代价，不把 GCFeed 的功能或数字写成 TokMall 的成果。
