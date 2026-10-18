# 性能证据与压测计划

本文回答"性能数字从哪来、在什么条件下成立、下一步要测什么"。原始数据在 [benchmarks/](../benchmarks/)，每个实验目录的 README 记录构建、参数和限制。2026-10-09 之前的观察缺少原始数据、无法复现，已从本文移除，需要时从 Git 历史查看。

## 读数规则

- 每个数字只对它的构建、机器和场景成立；所有实验都是 API、压测端、依赖容器同机运行，不是生产容量。
- 只有同机、同版本、只改一个变量、三轮交错的对照才能算优化收益。单轮结果只用来定位问题。
- 交易压测以"HTTP 成功 + 账本、支付、库存、回执、投影全部一致 + 九项对账通过"为成功标准，只看 HTTP 200 不算数。
- 正确性并发测试证明"不超卖、不重复"，不代表吞吐。
- 目前的实验都是固定并发的闭环模型：服务变慢，下一次请求也跟着变慢。所以它们能比较两个版本，但不能证明在固定到达率下积压能收敛。

<a id="latest-results"></a>

## 最近结果：2026-10-12 分步优化

条件：百万视频、三万用户，L1+L2 缓存开启，默认日志和 worker，隔离实例关闭限流，交易带视频来源和日销投影。每步只改一个主要变量。

| 步骤 | 改动 | 结果 | 结论边界 |
|---|---|---|---|
| [1 安全保留](../benchmarks/2026-10-12/step1-retention/README.md) | Stream 按确认水位裁剪、PG 背压、租约过滤；总池 25 | 50 并发单轮：热点 381.0 单/秒，100 SKU 943.4 单/秒；79,537 单回执/投影缺口为 0，九项通过 | 正确性验收，不是吞吐结论 |
| [2 消费预算](../benchmarks/2026-10-12/step2-consumer-budget/README.md) | 前台 20 / 消费 5，总上限不变 | 热点 355.7 单/秒，压后等待及对账 31.9 → 1.0 秒；100 SKU 794.5 单/秒，83.2 → 57.4 秒 | 缓解消费饥饿；HTTP 吞吐下降，不宣称收益 |
| [3 Feed 合并读取](../benchmarks/2026-10-12/step3-feed-dynamic/README.md) | 点赞与商品卡合并为一次 SQL | 100 并发 60 秒×3：吞吐中位数 6,484.0 → **9,269.6 请求/秒（+43%）**，P99 43.15 → 39.89 ms，零错误 | 严格三轮对照；仅限热缓存、稀疏点赞/商品数据的首页 |
| [4 库存临界区](../benchmarks/2026-10-12/step4-stock-lock/README.md) | 库存更新移到事务尾部 | 热点 50 并发 60 秒×3：中位数 **597.5 单/秒**、P99 213 ms（参考 355.7、385 ms）；六轮共 252,256 单，九项全过 | 优化前只有单轮参考，不是严格三轮 A/B；100 SKU 中位数 829.7，范围覆盖参考值，不主张改善 |

停压后消费者仍需要 30～60 秒追平积压：生产提速后，消费端是下一个瓶颈。

## 2026-10-11：完整版本基线与诊断

[基线证据](../benchmarks/2026-10-11/full-baseline/README.md)：构建 `33c29ea`，i5-13600KF（20 逻辑 CPU）/32GiB，PostgreSQL 18.6，连接池 25。

| 场景 | 结果 |
|---|---|
| Feed 首页，100 并发 60 秒×3 | 中位数 7,899.3 请求/秒，P99 33.81 ms，零错误；200/400 并发出现连接重置 |
| 1 SKU，10 并发 60 秒×3 | 中位数 377.5 单/秒，P99 84 ms，68,183 单全部闭环 |
| 1 SKU/50 并发、100 SKU/10 并发（30 秒） | HTTP 全部成功，但分别缺 940、841 个消费回执，对账失败 |

[诊断归档](../benchmarks/2026-10-11/diagnosis/README.md) 定位了三个结论，直接催生了 10 月 12 日的四步优化：

- **回执缺失。** 根因是 `XADD MAXLEN ~ 10000` 裁掉了未消费的消息。缺失事件的 Stream ID 全部早于保留边界；只把上限提高到 100 万，就能全部闭环，这证实了根因。
- **热点交易。** 锁等待采样全部指向同一库存行。Go block profile 显示约 49% 的阻塞时间花在取连接；等锁的事务占住了共享池，消费者只有约 27 事件/秒，而生产约 392 事件/秒。
- **Feed。** 约 88% 的阻塞时间在动态字段读取的取连接路径上。连接数加到 50 只改善了 P99，吞吐不变；冻结动态字段的诊断变体能到约 2.53 万请求/秒，但它不保证写后更新，只用于定位，不能上线。

## 2026-10-09：Feed 缓存 A/B/C

Mac17,3 arm64 10 核，50 并发，每轮预热 30 秒、测 180 秒（覆盖多次 60 秒 L2 过期），三轮按 ABC/CBA/ABC 交错。原始数据在 `benchmarks/2026-10-09/formal/`。

| 组 | 请求/秒（三轮中位数，范围） | P99 ms |
|---|---:|---:|
| A 直查 | 4,262.8（4,138.4～5,866.6） | 26.78 |
| B 仅 Redis | 7,111.6（6,742.5～7,247.5） | 20.82 |
| C L1+Redis | 8,327.2（8,234.6～8,883.9） | 17.21 |

C 相对 A 吞吐约 +95%，C 相对 B 约 +17%；同轮的 C/A 增幅在 51%～99% 之间漂移。缓存组仍有连接池等待，因为动态字段每次请求都查库。

同日的交易复测在 100 SKU/10 并发组对账失败后中止（27,985 个事件缺回执），这是最早发现 Stream 裁剪问题的实验；用 `reconcile --replay` 两次之后恢复到零差异。

<a id="plan"></a>

## 下一步：证明每项技术的占用是合理的

现有实验证明了"优化有效"，但还没回答"这个组件该不该存在"。每项实验都要能写成一句话：**"没有 X，在负载 Y 下会出现 Z；X 的代价是 W"**。写不出这句话的组件，应该删掉或降级。

实验方法上的要求：
- 用 k6 `constant-arrival-rate`（开环、固定到达率）逐级加压找拐点，再在拐点以下跑 30 分钟以上，确认积压收敛。
- 同时记录成本侧：内存、连接数、Redis 操作数、CPU，而不只是收益。
- 每轮压完照常跑对账。

| 要证明的组件 | 对照方案 | 负载 | 判定指标 |
|---|---|---|---|
| 异步投影 | 投影直接在支付事务里更新 | 单个热点作者持续支付 | 支付吞吐与 P99；同一作者同一天的投影行会成为新的热点锁 |
| Redis Streams | 消费者用 PG `SKIP LOCKED` 直接取 outbox | 固定到达率 300 单/秒，30 分钟 | 积压是否收敛、端到端事件延迟、PG 负载 |
| 日销投影读模型 | 实时聚合订单表 | 单作者 10 万～100 万订单 | 统计接口 P99，决定是否改读投影 |
| 订单超时延迟事件 | 只保留 5 秒轮询 | 10 万笔待支付订单 | 关单延迟分布、DB 扫描开销 |
| L2 + 版本号 | 只用 L1 | 两个实例，读写混合 | 命中率、实测陈旧窗口 |
| 布隆过滤器 / 负缓存 | 分别开关 | 随机不存在 ID 的穿透流量 | DB QPS、内存；预期布隆开关结果相同 |
| 热点库存 | 固定到达率逐级加压 | 1 个 SKU | 拐点；只有拐点低于目标时才考虑 Redis 预扣 |
| 周期对账 | 开关对照 | 1000 万订单 + 前台持续负载 | 对账耗时、前台 P99 增量 |
| 上传封存 | toxiproxy 让 RustFS 变慢 | 并发封存 512 MiB 文件 | 连接池等待、行锁持有时间 |
| Redis 故障 | 压测中停 Redis 60 秒 | 交易负载 | 下单是否继续、恢复后排空时间、对账结果 |

还没测过的场景：读写混合、关注流与深分页、热榜、券和退款压力、多实例。

<a id="how-to-run"></a>

## 如何运行

### 准备

- 压测实例使用专用 PG 库（脚本自动新建 `tokmall_bench_*`）和专用的空 Redis DB，API/指标端口为 18080/19091，并关闭限流。
- 不要在有业务数据的库上造数或压测。[seed_perf.sql](../scripts/seed_perf.sql) 使用模拟媒体状态。
- 用编译好的 `./bin/api`，不用 `go run`。

```bash
go build -o bin/api ./cmd/api
go build -o bin/reconcile ./cmd/reconcile
./bin/k6 version
BENCH_REDIS_DB=12
docker exec tokmall-redis redis-cli -n "$BENCH_REDIS_DB" DBSIZE   # 必须为 0，且确认没有其他程序在用
```

### k6 实验（当前方式）

[benchmark_k6.py](../scripts/benchmark_k6.py) 负责调度 [k6 场景](../scripts/k6-baseline.js)。每个案例会清理该实验的交易表和 Redis 缓存。

```bash
python3 scripts/benchmark_k6.py \
  --output .gocache/benchmarks/new-feed-run \
  --redis-db "$BENCH_REDIS_DB" \
  --suite feed --clients 100 --seconds 60 --rounds 3 --settle-seconds 180
python3 scripts/summarize_k6.py .gocache/benchmarks/new-feed-run benchmarks/new-feed-run
```

- 交易实验改用 `--suite commerce --clients 50`，会分别测 1 个和 100 个 SKU。
- `--resume` 只能用于相同 Git HEAD、相同二进制、相同实验资源的续跑。
- Feed 场景检查 HTTP 200 且返回 20 项；交易场景下单后再支付，两步都成功才计一单。

每轮保存 k6 汇总、逐请求样本、指标前后快照、PG 统计、进程资源和对账结果。较早的 [benchmark_backend.py](../scripts/benchmark_backend.py) 和 [loadgen](../cmd/loadgen/main.go) 用于 10 月 9 日的实验，命令见 [benchmarks/README](../benchmarks/README.md)。

<a id="lock-waits"></a>

### 现场查数据库锁等待

压测期间连接本轮 `environment.json` 里记录的库：

```sql
SELECT pid, state, wait_event_type, wait_event,
       pg_blocking_pids(pid) AS blockers,
       now() - xact_start AS transaction_age,
       left(query, 300) AS query
FROM pg_stat_activity
WHERE datname = current_database() AND pid <> pg_backend_pid() AND state <> 'idle'
ORDER BY xact_start NULLS LAST;
\watch 0.2
```

读数注意：
- 保持自动提交，不要包在 BEGIN 里。
- 阻塞者当前显示的 SQL 可能已经换成后续语句，但锁要到提交才释放。
- 事务年龄不等于锁等待时间。

连接池指标用 `curl -s http://127.0.0.1:19091/metrics | grep '^db_pool_'` 查看：
- 按 db 标签区分前台池和消费池；
- WaitCount/WaitDuration 要看同一进程窗口内的增量；
- 池满不等于应该加连接。

<a id="profiles"></a>

### Go profile

普通 API 不开 pprof。10 月 11 日的诊断用 build overlay 在 `127.0.0.1:19092` 加了 pprof，开启了 block/mutex 采样，工具源码在诊断目录的 `diagnostic-tools.tar.gz`。可以直接读归档文件练习：

```bash
DIAG=benchmarks/2026-10-11/diagnosis/commerce-c50-trim10k-r2
go tool pprof -top -cum "$DIAG/block.pprof"
go tool pprof -top "$DIAG/profile.pprof"
```

几种 profile 各看什么：
- CPU profile 看处理器开销；
- block profile 看 goroutine 阻塞，其中 `DB.conn` 的占比是累计阻塞时间，不是 CPU 占比；
- mutex profile 看 Go 锁竞争；
- 数据库行锁仍需用上面的 PG 现场采样说明。

`EXPLAIN (ANALYZE, BUFFERS)` 只反映 SQL 执行成本，不包含应用取连接的排队时间。
