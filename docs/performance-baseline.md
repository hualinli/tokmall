# 性能基线（M3 末）

> **历史测量记录**：首轮记录于 2026-08-06，后续章节记录各自测试时点。下文数字不能当作当前版本的性能承诺；早期“下一步做”是当时结论，现状见 [文档导航](README.md) 与 [项目记忆](../AGENTS.md)。跨版本压测需保持数据规模、部署形态和请求组合一致。
>
> ⚠️ **环境注记**：下文多处的 5433 e2e 实例（`tokmall-postgres-e2e`）是当时本机 5432 被 go-oj 临时占用时
> 手动 `docker run` 的容器，**不是 docker-compose 标准配置**（标准为 5432）。历史记录保留原样以保真；
> 复现压测请用 compose 的 5432 实例。

## 方法

| 项 | 值 |
|---|---|
| 工具 | wrk 4.2.0（kqueue） |
| 参数 | `-t4 -c50 -d10s` |
| 机器 | 本机 macOS（Apple Silicon） |
| 服务 | 单实例 `go run ./cmd/api`（debug 构建，非 -ldflags 优化） |
| 数据库 | PostgreSQL 16 Docker 容器（e2e 实例，5433），连接池 max_open=25 |
| 数据量 | videos 表 7 条 PUBLISHED 记录（**注意：不是百万级数据**） |

> ⚠️ 数据量警告：7 条数据压的是"框架开销 + 连接池吞吐"，不是真实查询性能。
> 后续 M4 对比已按百万条数据另行测量；本段 7 条记录的数字只保留为当时的框架/连接池基线。

## 结果

| 接口 | QPS | 平均延迟 | 备注 |
|---|---|---|---|
| `GET /api/v1/feed`（未缓存） | **12,219** | 4.9ms | 压测期间 db_pool_wait_count 累计 ~12 万，25 连接接近饱和 |
| `POST /api/v1/auth/login` | **84.7** | 545ms | bcrypt cost 10 为瓶颈，~70ms/次哈希 |

## 当时的压测命令

```bash
# feed
wrk -t4 -c50 -d10s http://localhost:8080/api/v1/feed

# login（POST + JSON body）
cat > /tmp/login.lua <<'EOF'
wrk.method = "POST"
wrk.path = "/api/v1/auth/login"
wrk.headers["Content-Type"] = "application/json"
wrk.body = '{"identifier":"alice12","password":"password123"}'
EOF
wrk -t4 -c50 -d10s -s /tmp/login.lua http://localhost:8080
```

## 观察与结论

1. **读路径**：12k QPS 无缓存，池接近饱和但延迟稳定——M4 引入 Redis 后对比目标：QPS 提升 + db_pool_wait_count 下降。
2. **登录**：~85 QPS 是 bcrypt 的设计代价（防暴力破解优先）。优化候选：cost 降到 8、换 Argon2id、或接受现状（冷路径）。**不要**在没有业务压力的情况下为压测数字动认证成本。
3. **指标链路已验证**：hertz throughput 计数器与 wrk 数字吻合（122,523 ≈ 12,219×10），metrics 可信。

---

# M4 缓存对比（2026-08-07）

> 同一台机器、同一个 e2e 容器、同样的 100 万条数据，唯一变量是 FEED_CACHE_ENABLED。

## 方法

| 项 | 值 |
|---|---|
| 工具 | vegeta（`-duration=10s -rate=0 -max-workers=50`，无限速） |
| 数据量 | videos 表 **100 万条 PUBLISHED**（`scripts/seed_perf.sql` 造数），30k 用户 |
| 场景 A | `FEED_CACHE_ENABLED=false`（无缓存，直查 PG） |
| 场景 B | 缓存开（TTL=60s），压测前已 warm |
| 缓存命中 | `feed_cache_hit_total` / `feed_cache_miss_total`（:9091/metrics） |

## 结果

| 场景 | QPS | p50 | p90 | 最大 | db_pool_wait_count | feed_cache 命中率 |
|---|---|---|---|---|---|---|
| M3 基线（7 行，无缓存） | 12,219 | - | - | - | ~120k | - |
| **1M 无缓存** | **6,576** | 5.4ms | 14.1ms | 220ms | **58,706** | - |
| **1M 有缓存** | **11,337** | 3.1ms | 6.9ms | 218ms | **0** | **99.999%**（113,378/1） |

## 观察与结论

1. **1M 数据让 DB 路径慢了 ~46%**（12.2k → 6.6k QPS）：索引深度 + 数据体积，符合预期。M3 的"7 行基线"确实不能代表真实规模。
2. **缓存把 DB 压力归零**：db_pool_wait_count 从 58,706 → 0，连接池从瓶颈变成空闲。
3. **缓存后的新瓶颈是 Redis GET + JSON 反序列化**（~11.3k QPS 封顶，命中路径每次请求一次 Get + 一次 Unmarshal）：对比 7 行基线还略低，说明现在测的是 Redis 吞吐而非 DB。进一步优化方向（不进 M4）：命中时直接返回原始字节、或预序列化 bytes 缓存。
4. **99.999% 命中率是 10s 压测的假象**：TTL 60s 意味着每 60s 有一次 miss + 回填。真实流量下周期性击穿的防护靠 singleflight（已实现）+ double-checked locking。
5. **压测结论**：M4 目标达成——QPS 提升 1.72x（6.6k→11.3k），延迟减半，DB 完全脱出热路径。

## 当时的压测命令（5433 临时容器与旧二进制名）

```bash
# 造数（幂等）
docker exec -i tokmall-postgres-e2e psql -U tokmall -d tokmall < scripts/seed_perf.sql

# 场景 A：无缓存
APP_ENV=development FEED_CACHE_ENABLED=false DB_PORT=5433 JWT_SECRET=test MINIO_ENDPOINT=localhost:9000 ./tokmall-api &
vegeta attack -targets=/tmp/feed-targets.txt -duration=10s -rate=0 -max-workers=50 | vegeta report

# 场景 B：有缓存（先 warm 两次再压）
APP_ENV=development DB_PORT=5433 JWT_SECRET=test MINIO_ENDPOINT=localhost:9000 ./tokmall-api &
curl -s localhost:8080/api/v1/feed > /dev/null && curl -s localhost:8080/api/v1/feed > /dev/null
vegeta attack -targets=/tmp/feed-targets.txt -duration=10s -rate=0 -max-workers=50 | vegeta report
```

---

# M6 关注 Feed 压测基线（2026-08-11）

> 同一台机器、同一个 e2e 容器。**个性化接口**：50 用户 token 轮换（vegeta target 文件），
> 关注数分档造数（`scripts/seed_follows.sql`，幂等）。关注 Feed 无缓存（个性化未做缓存）。

## 方法

| 项 | 值 |
|---|---|
| 工具 | vegeta（`-duration=10s -rate=0 -max-workers=50`，与 M4 同口径） |
| 端点 | `GET /api/v1/feed/following?limit=20`（Bearer 认证） |
| 数据 | 30k 用户 / 1M 视频 / follows 分档（0/10/100/1000，各 50 压测用户） |
| token | 本地 HS256 签发（`JWT_SECRET=e2e-secret`，sub=seed 用户 id） |
| 服务 | `./bin/api`（debug 构建，time 模式；场景 E 为 hot 模式） |

## 结果

| 场景 | 关注数 | QPS | p50 | p90 | p99 | 备注 |
|---|---|---|---|---|---|---|
| A | 0（空短路） | **20,414** | 1.7ms | 4.2ms | 12.5ms | 一次索引查询 + 空返回，纯框架开销 |
| B | 10 | 4,813 | 9.7ms | 14.4ms | 23.6ms | |
| C | 100 | 1,601 | 29.4ms | 44.7ms | 64.5ms | |
| D | 1000（上限） | **204** | 240ms | 353ms | 490ms | p50 是 DB 查询时间的 10 倍（连接池排队） |
| E | 100 + HotRanker | 1,171 | 40.9ms | 57.5ms | 81.1ms | +PlayCounts/LikeCounts 聚合，-27% QPS |
| 对照 | 全局 Feed 无缓存（M4） | 6,576 | 5.4ms | 14.1ms | - | 匿名 + 单表查询 |
| 对照 | 全局 Feed 缓存（M4） | 11,337 | 3.1ms | 6.9ms | - | |

## 瓶颈分析（EXPLAIN + 指标）

1. **关注数增长 → 超线性退化**：10→1000 关注，QPS 从 4.8k 崩到 204（23 倍）。1000 关注（手册设定上限）当前不可用。
2. **SQL 结构不是主瓶颈**：窗口函数（扫 34k 行截断 5k）与 LATERAL 每作者 LIMIT 5（5k 行、页读少 4.5 倍）实测耗时相近（22.8 vs 27.2ms）——1000 次索引 seek 的迭代开销抵消了页读收益。
3. **每请求 3 次 DB 往返 + 连接池排队是主因**：`ListFollowingIDs`（0.2ms）+ `ListFollowingCandidates`（22.8ms）+ `LikeCounts`（20 条 IN GROUP BY）+ assemble 40 次预签名（本地 HMAC，CPU）。25 连接池 × 25ms/请求 ≈ 理论 1000 QPS，实测 204 说明还有排队放大。
4. **空短路设计有效**：0 关注 20k QPS，框架开销可忽略——退化完全来自查询路径本身。
5. **db_pool_wait_count 压测期间累计 ~115 万**：连接池硬饱和（与 M3 基线同症状，这次是关注 Feed 独有的查询重）。

## 当时的决策（由数据驱动，不在本次实现）

| 候选 | 预期收益 | 判断 |
|---|---|---|
| **关注 Feed 第一页缓存**（个性化 key，TTL 30s，cache-aside + TTL 兜底，与 ADR-012 同哲学） | 当时预计：3 次 DB + 40 次签名 → 1 次 Redis GET，100 关注从 1.6k → >5k | 当时选定；当前已实现，不能把预计数字当作实测 |
| 合并 `ListFollowingIDs` 与候选查询（IN 子查询重复查 follows） | 小（省 0.2ms/请求） | 顺手做 |
| LATERAL 替代窗口函数 | 无显著收益（27 vs 22.8ms） | ❌ 不值得，窗口函数可读性更好 |
| 关注列表缓存（Redis） | 中，但结果缓存已覆盖 | 依赖上一个 |
| 加连接池 | 掩耳盗铃（根因是每请求 DB 时间） | ❌ |

## 当时的压测命令（5433 临时容器）

```bash
# 造数（幂等）
docker exec -i tokmall-postgres-e2e psql -U tokmall -d tokmall < scripts/seed_perf.sql
docker exec -i tokmall-postgres-e2e psql -U tokmall -d tokmall < scripts/seed_follows.sql

# 起服务 + 压测（target 文件生成见下）
APP_ENV=development DB_PORT=5433 JWT_SECRET=e2e-secret MINIO_ENDPOINT=localhost:9000 ./bin/api &
vegeta attack -targets=/tmp/feedbench/follow1000.targets -duration=10s -rate=0 -max-workers=50 | vegeta report

# token/target 生成（python3，HS256，sub=seed 用户 id 9..208）
# 档位: follow0=id 159..208, follow10=109..158, follow100=59..108, follow1000=9..58
```

---

# M6 关注 Feed 缓存对比（2026-08-11，同日）

> 同一机器、同一 e2e 容器、同一 target 文件，唯一变量是关注 Feed 第一页缓存
> （`FEED_FOLLOWING_CACHE_ENABLED`，TTL 复用 60s，key 带 user_id + rank_version）。

## 结果

| 场景 | 关注数 | 无缓存 QPS | 有缓存 QPS | p50 无→有 | 提升 |
|---|---|---|---|---|---|
| A | 0 | 20,414 | **38,336** | 1.7→1.2ms | 1.9x（空结果也缓存，纯 Redis GET） |
| B | 10 | 4,813 | 6,995 | 9.7→4.6ms | 1.45x（DB 本身轻，收益被 Redis+LikeCounts 吃掉） |
| C | 100 | 1,601 | 6,977 | 29.4→4.5ms | **4.4x** |
| D | 1000（上限） | 204 | 6,578 | 240→4.7ms | **32x** |

## 观察与结论

1. **缓存把"关注数"这个维度彻底抹平**：10 关注和 1000 关注命中缓存后 QPS 几乎相同
   （7.0k vs 6.6k）——扩展性问题从"查询复杂度"变成"每请求固定开销"。
2. **1000 关注从不可用到与全局 Feed 缓存同档**：204 → 6.6k QPS，p50 240ms → 4.7ms。
   这是本次优化的核心收益：手册设定上限（1000 关注）不再是性能悬崖。
3. **命中路径的固定开销**：1 Redis GET（反序列化）+ 1 LikeCounts（20 条 IN GROUP BY）
   + 20 条响应序列化 ≈ 6.6-7k QPS 封顶。对比全局 Feed 缓存 11.3k 的差距正是
   LikeCounts 实时查询 + 更大响应体（个性化 URL）。
4. **场景 A 缓存后 38k QPS**：空结果缓存后连 DB 都不碰（此前空短路查一次 follows），
   证明 Redis 纯 GET 吞吐远高于 DB 路径。
5. **一致性代价**：TTL 60s 内关注/取关/发布不即时反映（与首页缓存 ADR-012 同哲学，
   失效成本是不值得的——个性化 key 需要反向索引 follower 列表）。
6. **db_pool_wait_count 压测增量 ~12 万**（无缓存版全场景累计 115 万）：DB 压力大降。

## 决策

| 项 | 结论 |
|---|---|
| 关注 Feed 第一页缓存 | ✅ 已实现（key 带 rank_version 防排序切换串数据） |
| 翻页缓存 | 不做（命中率低，与首页缓存同取舍） |
| like_count 进缓存 | 不做（高频写，失效风暴，与首页缓存同取舍） |
| 关注列表缓存 / SQL 重写 | 不需要（缓存后不再是瓶颈） |

---

# 播放事件压测与 Kafka 决策（2026-08-13）

> 目标：验证"同步写 play 拖慢读路径"（M5-Kafka 的引入动机）是否成立。
> 方法：play 是 POST + 每请求唯一 event_id（vegeta 不支持动态 body，用 python 并发脚本），
> feed 用 vegeta。限流关闭（RATE_LIMIT_ENABLED=false，避免干扰）。单实例 debug 构建。

## 结果

| 场景 | QPS | p50 | p90 | p99 | 说明 |
|---|---|---|---|---|---|
| 读 feed（follow0 缓存命中） | 37,946 | 1.2ms | 1.8ms | 2.4ms | 纯 Redis GET（空结果无 LikeCounts） |
| 读 feed（follow1000 缓存） | 6,249 | 5.2ms | 13.7ms | 22.3ms | 每请求 1 次 LikeCounts DB 查询 |
| 写 play（40 workers 单独） | 1,640 | 17.2ms | 20.9ms | 25.2ms | 真实 INSERT（16,400 条入表） |
| **混合：feed + play 并行** | feed 5,840 / play 528 | 5.5 / 29.3ms | 14.8 / 45.4ms | 29.1 / 75.7ms | db_pool_wait 40k→179k |

## 结论（数据驱动）

1. **"写拖慢读"证伪**：混合场景下 feed 读只降 6.5%（6,249→5,840），p50 基本不变。
   原因：feed 命中缓存后每请求只占一次 LikeCounts 短查询；play 写虽然每条 INSERT 17ms
   占连接池，但读路径的 DB 占用极短，写排队没有传导到读延迟。
   **缓存（ADR-012 + 关注 Feed 缓存）已经把读写路径隔离了。**

2. **真实瓶颈是写吞吐自身**：play 单独 1.6k QPS，混合下被拖到 528 QPS（连接池竞争）。
   若播放量真到每秒数千次，play 接口自身会先成为瓶颈（排队/超时），而不是拖慢读。

3. **Kafka 决策：继续搁置**。M5-Kafka 的动机（写阻塞读）在当前架构下不成立；
   引入 Kafka 的前提是"写吞吐成为可测量瓶颈"（每秒数千播放量级），学习项目达不到。
   若将来要做，正确叙事是"异步化提升写吞吐 + 削峰"，而非"防止拖慢读"。

4. **play 写降本的候选（若未来需要）**：删 play_events FK（ADR-013 已评估为可讨论）、
   批量攒批落库（本地队列即可，不必 Kafka）、event_id 用 DB 生成的紧凑主键。

5. **压测方法论记录**：vegeta 不支持每请求动态 body（target body 是静态的），
   play 类写压测需脚本生成唯一幂等键——这是后续写压测的通用约束。

## 可复现

```bash
python3 /tmp/feedbench/play_bench.py 40 10   # play 写压测（每请求唯一 event_id）
vegeta attack -targets=/tmp/feedbench/follow1000.targets -duration=10s -rate=0 -max-workers=50  # feed 读
# 混合：两个并行
```

---

# 下单/支付链路基线（2026-10-01，Commerce v2 + Outbox）

## 方法

新增 `cmd/loadgen`：下单链路不是"打一个 URL"，而是有状态流程
（注册 → 建商品/SKU → 补库存 → 并发下单 → 支付），且必须区分
"业务失败"（库存不足、幂等冲突）与"系统失败"（5xx/超时）。
wrk/vegeta 无法表达这套语义，因此自写客户端。

```bash
make build
RATE_LIMIT_ENABLED=false ./bin/api &     # 压测时关闭限流（写接口 100/10s/用户会先挡住）
./bin/loadgen -scenario commerce -concurrency 50 -duration 20s -stock 1000000
```

环境：本机 macOS（x86_64），PostgreSQL 16 / Redis 7 / MinIO 均为 Docker 容器，
API 与压测客户端同机。连接池 max_open=25。**这些数字只代表本机学习环境，
不是容量承诺。**

## 结果

| 指标 | 数值 |
|---|---|
| 并发 worker | 50 |
| 时长 | 20s |
| 总请求 | 18,228（下单 9,114 + 支付 9,114） |
| 成功 | 18,228（100%） |
| 业务失败 / 系统失败 | 0 / 0 |
| 吞吐 | 911 req/s（约 455 单/s） |
| p50 / p95 / p99 / max | 39.4ms / 153.7ms / 236.6ms / 786.7ms |

数据一致性（压测结束后核对）：

| 核对项 | 结果 |
|---|---|
| 库存 | 1,000,000 − 9,114 = 990,886（与成功下单数**完全一致**，无超卖、无丢更新） |
| Outbox 积压 | `PENDING = 0`（中继跟得上写入） |
| 消费回执 | 20s 后追平全部事件（消费峰值落后约 2k 条，随后补齐） |
| 对账 | `bin/reconcile` 六项全 OK |

## 观察与结论

1. **p50 39ms 不是数据库慢，而是"每单两次串行往返 + 连接池竞争"**：
   下单与支付是两个独立事务，50 个 worker 抢 25 条连接，等待时间进了 p95/p99。
   这也解释了 p99/p50 ≈ 6 倍的长尾。
2. **限流会先于数据库成为瓶颈**：未关闭限流时，50 并发下 44,001 次请求被
   429 挡回（每用户 100 次/10s）。这是**设计如此**：写接口的配额按用户，
   压测要么提高用户数，要么显式关闭限流。这条观察本身说明限流确实生效。
3. **Outbox 没有把写路径拖垮**：事件与订单同事务插入，压测期间积压为 0，
   说明"多写一张表 + 异步投递"在这个量级下是可接受的成本。
4. **当前瓶颈排序**：连接池（25）> 单请求事务往返 > CPU。
   若要把下单链路推到数千 QPS，先做的是提高连接池并合并下单/支付往返，
   而不是引入消息队列。

---

## 观测栈（本地可选启动）

```bash
make dev-backend     # 应用指标在 127.0.0.1:9091
make monitoring      # Prometheus(:9090) + Grafana(:3000)，看板已 provision
```

- `observability/prometheus/prometheus.yml`：抓取应用自身（`tokmall-api` job）+ Prometheus；
- `observability/prometheus/rules/tokmall.yml`：8 条记录规则（`level:metric:operations` 命名）
  + 7 条告警（Outbox 积压/死信、对账差异、对账停摆、5xx 比例、连接池饱和、对象存储熔断）；
- `observability/grafana/dashboards/tokmall-overview.json`：11 个面板（请求量与 P95、
  5xx、连接池、缓存命中率、Outbox 积压与延迟事件、投递失败率、对账、熔断、穿透防护、
  订单超时事件结果）——**面板 JSON 提交进仓库**，不是"去 grafana.com 导个 ID"。

**两个从验证里发现的告警陷阱**（都已修）：
1. `reconcile_last_run_ok` 的零值有歧义——0 既表示"发现差异"也表示"还没跑过"，
   只看 `== 0` 会让**每次重启都误报对账不一致**。正确写法是叠加
   `and reconcile_last_run_timestamp_seconds > 0`。
2. 反过来的失效方式更隐蔽：对账**根本没跑**时指标没有数据，任何 `== 0` 类告警都不会触发。
   因此补了一条 `TokMallReconcileStale`（30 分钟没有运行即告警）——
   没有对账就没有"账实一致"的证据，比报差异更危险。

**规则不是摆设**：`internal/observability/rules_test.go` 会把规则文件里引用的每个标识符
与真实注册的指标集合对账，写错指标名（规则永远不会触发）会让测试直接失败。
这个检查是变异验证过的：把 `outbox_pending_events` 改成 `outbox_pending_evnts`
或换成不存在的指标，测试都会报错并指出具体行。

---

## 两级缓存（L1 + L2，2026-10）

| 层 | 位置 | 命中成本 | TTL | 失效方式 |
|---|---|---|---|---|
| L1 | 进程内分片 LRU | 一次内存查表 + 反序列化 | 5s（`FEED_L1_TTL`） | 本地删除 + 版本号校验 |
| L2 | Redis | 网络往返 + 传输 + 反序列化 | 60s（`FEED_CACHE_TTL`） | 直接删除 |

跨实例失效用"版本号 + 短 TTL 探测"（ADR-025）：写路径删两级缓存并 `INCR feed:version`，
读路径用本地缓存的版本号（默认 1 秒刷新）校验 L1 副本。指标：

- `cache_l1_hit_total / miss_total`：L1 命中率；
- `cache_l1_stale_total`：因版本变化失效的次数——它直接反映"别的实例写了新数据"的频率；
  若 stale 与 hit 同量级，说明写入太频繁、L1 没起到作用（该调大 TTL 或干脆关掉）；
- `cache_l1_eviction_total`：LRU 淘汰，容量不够的信号。

实测（本机 5 次连续请求）：首次冷启动未命中，之后 4 次全部 L1 命中，
`cache_l1_hit_total=4 / miss_total=2`，feed 响应时间从 ~9ms 降到 ~2.5ms。

## Redis 慢命令观测（2026-10）

`internal/cache/slowlog.go`：go-redis Hook，命令耗时 ≥10ms 记一条 WARN
（命令名 + 耗时 + **key 前缀**，不打完整 key）并计入 `redis_slow_commands_total{command}`。

排除阻塞命令（`XREADGROUP`/`BLPOP`/`SUBSCRIBE` 等）——实测踩到：Outbox 消费者的
`XREADGROUP ... BLOCK 5000` 让慢命令计数持续增长，看起来像 Redis 变慢，
其实是消费者在正常等待（ADR-026）。
