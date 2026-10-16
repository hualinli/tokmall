# 开发、验证与运维

> 核对日期：2026-10-13；命令与默认值对照 [Makefile](../Makefile)、[配置](../internal/config/config.go)、[CI](../.github/workflows/ci.yml)。这里说明怎么运行，业务规则与机制分别见 [商城业务](commerce-business.md)、[后端架构](backend-architecture.md) 和 [商城技术实现](commerce-v1-technical-design.md)。

## 环境与启动

准备Go（要求见go.mod）、Node 22、pnpm 11和运行中的Docker/Compose。首次运行按下方命令；业务演示步骤在 [根README](../README.md#第一次演示怎么走)。真实媒体验证需ffprobe/ffmpeg。依赖使用标准PG5432、Redis6379、RustFS9000/9001端口，不另建5433实例。

```bash
cd web && pnpm install --frozen-lockfile
cd ..
make dev
# 前端 http://localhost:5173，API http://localhost:8080
# OpenAPI http://localhost:8080/api/v1/docs/openapi.yaml
# 指标 http://127.0.0.1:9091/metrics
make logs
make stop
```

`make dev` 起依赖容器、编译后端、启动前端；日志在 `logs/api.log`、`logs/web.log`。`make stop` 的实现是匹配 `bin/api`/Vite 并强制停止，不等于验证优雅停机；应用本身有信号停机逻辑。避免按 8080 端口批量杀进程，本机可能还有其他应用。要验证优雅停机，对明确的后端 PID 发送 SIGTERM 并检查 worker 退出日志。

Docker 凭证助手异常时，按 [项目记忆](../AGENTS.md) 的 `.docker-anon/config.json` 配置绕过；手动容器命令可用 `DOCKER_CONFIG=$PWD/.docker-anon docker compose up -d postgres rustfs redis`。`make dev` 自身未硬编码匿名目录，需要正确的 Docker 环境。

直接启动任何 Go 工具都需要 `APP_ENV`、`JWT_SECRET`、`FEED_CURSOR_SECRET`，即使该工具只连数据库。Makefile 仅为本地开发提供密钥。生产还校验 DB/Redis/RustFS 显式配置、TLS、密钥长度与占位凭证，完整条件看 [Validate](../internal/config/config.go)。

## 媒体与旧版本升级

默认 `MEDIA_PROBE_ENABLED=true`。ffprobe/ffmpeg 缺失与坏文件不同：缺 ffprobe 记 SKIPPED；坏文件记 REJECTED；探测超时/下载暂时失败延迟重试。只有 PASSED/SKIPPED 能发布，因此正常上传后要等 worker，作者个人页和视频详情显示校验状态与安全的拒绝原因，PENDING 时每 5 秒刷新（页面在后台时不持续轮询），PASSED/SKIPPED 且待发布才启用发布按钮；原始探测诊断不对外返回。

纯业务模拟／现有 E2E 的合成媒体可以显式关闭探测；此模式不证明媒体准入能力：

```bash
MEDIA_PROBE_ENABLED=false make dev-backend
```

[migrate](../internal/database/migrate.go) 在 API 启动时执行版本化 SQL，禁止 AutoMigrate。当前迁移到 000024：

| 迁移 | 影响 |
|---|---|
| 000021 | 退款后可重用券，保留历史引用；已有重用历史时不能恢复旧唯一约束 |
| 000022 | 旧 PENDING + PUBLISHED 媒体回到 PENDING_REVIEW，增加重试时间和发布门禁 |
| 000023 | 历史 DELETED 视频回补对象清理任务；旧未核销上传票证失效 |
| 000024 | 释放历史取消／过期订单占券；约束只覆盖仍占用券的订单，重用后回滚可能被拒 |

升级旧部署前停止旧进程，等旧 PUT URL 的最长有效期过去（默认 15 分钟），再启动新版本，不能用滚动发布声称旧签名已撤销。客户端重新申请上传票证。迁移回滚不等于恢复业务状态，先检查备份与迁移条件；本仓库没有独立的完整 up/down 兼容性流水线。

回滚000023前，先停止上传/删除视频等写入流量，保持清理worker运行，确认 `SELECT count(*) FROM object_delete_jobs` 为0后再停止应用并回滚。down会直接删除队列表，尚未完成的删除意图无法保留；存储不可用或任务仍在重试时不要回滚，也不要通过清空表绕过排空要求。

API 请求体上限为 64 KiB，认证接口额外限制为 16 KiB（超限返回 413，即使关闭限流仍生效）；媒体本体直接上传对象存储。认证路由先经过全局 read 配额、请求体上限与 IP 配额；只有登录和注册再检查账号标识符配额，refresh 与 logout 不携带标识符，只受 IP 配额约束。账号先按既有规则规范化，再以 SHA-256 摘要作为 Redis key，避免超长原文 key；邮箱和用户名仍是独立桶。升级会换用新账号桶，旧桶最多保留原 60 秒 TTL，不批量清除 Redis。修改 Compose 后需重建对应容器才会更新已运行的端口绑定；不能只修改文件后声称暴露已消除。

<details>
<summary>依赖端口、数据卷与旧对象存储迁移</summary>

Go 版本以 [go.mod](../go.mod) 为准；Node 22、pnpm 11 与 CI 对齐。Compose 提供 PostgreSQL `localhost:5432`、Redis `localhost:6379`、RustFS `:9000`（控制台 `:9001`），服务名分别为 postgres/redis/rustfs。不要另建 5433 实例。PostgreSQL 与 Redis 使用命名卷 `<项目名>_pg-data`、`<项目名>_redis-data`（默认项目名为 tokmall），重建容器不清空数据；清空全部本地数据需显式执行 `docker compose down -v`，它同时会删除 `tokmall_rustfs-data`。旧匿名卷不会自动迁移。首次切换前停止业务写入，备份 PG（如 `pg_dump`）与 Redis 的持久化数据，切换后恢复并检查业务记录、Outbox 和刷新令牌状态；仅备份而未恢复的新卷仍是空数据。不要在验证恢复前删除旧卷。

对象存储默认使用固定版本 `rustfs/rustfs:1.0.1`，应用通过通用 `S3_*` 配置访问，客户端库仍是 `minio-go`（S3 SDK，不要求MinIO服务端）。原 `MINIO_*` 环境变量须改名为对应 `S3_*`，包括三个URL有效期；生产校验也使用新变量。Compose中PG、Redis、RustFS API/控制台及观测栈端口均仅映射到127.0.0.1，浏览器CORS允许localhost/127.0.0.1的5173端口，部署到其他域名时显式更新 `RUSTFS_CORS_ALLOWED_ORIGINS`。新卷为 `rustfs-data`；不复用或转换旧 `minio-data`，本次迁移不包含历史对象数据。


</details>

## 输入长度与日志口径

业务标题、描述、规格、关键词、券名、退款文本、订单幂等键和支付身份字段在 service 按 Unicode 字符计数，对齐 OpenAPI maxLength 与 VARCHAR(n)。口令上限按 UTF-8 字节计 72（bcrypt 限制），邮箱上限按字节计 254；不能把这些例外改成字符计数。错误字段 last_error 按字节限制大小但在 UTF-8 边界截断，见 [用户校验](../internal/user/service.go)、[商品校验](../internal/product/service.go)、[订单校验](../internal/order/service.go) 与 [Outbox 仓储](../internal/outbox/repository.go)。

Redis 慢命令日志记录命令、耗时和业务 key 前缀，避免完整账号/订单标识；XREAD/XREADGROUP 等预期阻塞命令不计慢命令，防止把正常等消息误判为依赖延迟，见 [slowlog.go](../internal/cache/slowlog.go) 和 [测试](../internal/cache/slowlog_test.go)。

## 测试矩阵

默认 `go test ./...` 会跳过真实依赖测试，不能把默认通过描述成验证了数据库／消息／对象存储。启用Redis集成测试需使用专用测试实例：其中 [热榜测试](../internal/feed/hot_cache_test.go) 固定使用DB9，并在前后执行FlushDB；不能让DB9承载需保留的数据。

```bash
go test ./...
make lint          # golangci-lint 版本对照 CI 与 .golangci.yml
make verify        # fmt 会修改格式；vet/lint/test/tidy-check
scripts/preflight.sh
```

`preflight.sh` 检查格式、vet、测试、tidy 漂移，依赖安装后检查前端类型；它不执行 lint、前端生成物漂移检查或 E2E，不能替代完整 CI。

```bash
docker compose up -d postgres redis rustfs
TOKMALL_RUN_PG_INTEGRATION=1 \
TOKMALL_RUN_REDIS_INTEGRATION=1 \
TOKMALL_RUN_S3_INTEGRATION=1 \
TOKMALL_TEST_DATABASE_DSN='postgres://tokmall:tokmall@localhost:5432/tokmall?sslmode=disable' \
TOKMALL_TEST_REDIS_ADDR=localhost:6379 \
go test ./... -count=1
```

PG 测试用隔离 schema；多数Redis测试使用自己的key/stream，热榜DB9例外如上。不要在共享实例盲目FLUSHDB。S3 测试开关覆盖 [真实存储契约](../internal/storage/upload_integration_test.go)：上传长度/类型签名、ETag封存、防覆盖、下载/回写、Range、CORS、签名到期与幂等删除。默认连接本地RustFS开发凭证，可用 `S3_ENDPOINT/S3_ACCESS_KEY/S3_SECRET_KEY` 覆盖；每次建临时bucket并清理自身对象。各测试的依赖条件以测试源文件为准。

<details>
<summary>历史环境验收记录（2026-10-09；不代表当前全套结果）</summary>

2026-10-09迁移验收在RustFS 1.0.1、PostgreSQL 18.4、Redis 7.4的独立测试环境执行：显式开启三个真实依赖开关的全量Go测试通过，storage/config/observability竞态检查、lint、tidy一致性通过；前端类型、构建、30项单测及17项Chromium E2E通过。E2E关闭限流与媒体探测，验证真实直传、发布和Feed播放；媒体工具实测不属于本次迁移验收。CI使用PostgreSQL16，仍需以远端运行结果确认该平台门禁，不把本地结果标为远端CI已通过。

2026-10-09 Redis 7.4→8.10.2 与端口6380→6379复验在RustFS 1.0.1、PostgreSQL 18.6、Redis 8.10.2执行：三个真实依赖开关的全量 `go test ./... -count=1` 通过。重点核对了项目实际使用的Redis 命令面——`SET NX PX` 原子Claim、`INCR` 版本号、限流Lua（INCR+EXPIRE 首次置窗）、`EXPIRE` 分钟桶，以及 Streams 侧 `XGroupCreateMkStream` 的 BUSYGROUP 幂等、`XReadGroup` 阻塞读与 `redis.Nil` 超时识别、`XInfoGroups` 的 pending/lag 解析、`XPendingExt` 的 RetryCount 与认领递增、`XAutoClaim` 三返回值、`XAck`；行为与 7.4 一致，未发现版本差异。outbox 集成测试覆盖了 XAUTOCLAIM 重认领、DLQ 与幂等消费。性能数字仍属历史条件，不因本次版本变更重算。


</details>

并发检查示例：`go test -race ./internal/order ./internal/video ./internal/feed ./internal/outbox ./internal/user`。启用上述环境变量后也会运行相应真实依赖分支；全量 race 不是每次改文档都需要运行。

受限沙箱可将 Go 缓存放仓库内：

```bash
export GOPATH="$PWD/.gopath" GOMODCACHE="$PWD/.gopath/pkg/mod"
export GOCACHE="$PWD/.gocache" GOSUMDB=off
```

前端生成、类型、单测与 E2E 看 [web README](../web/README.md)。[CI](../.github/workflows/ci.yml) 有 backend/contract/web/e2e 四个 job，PG/Redis/RustFS 服务显式开启真实依赖测试；E2E 使用另起的RustFS且关闭媒体探测。[Security](../.github/workflows/security.yml) 独立执行 CodeQL/govulncheck。CI 跳过纯 Markdown 改动，文档链接需要单独核验。

## 观测与恢复

`GET /health/live` 恒返回 OK；`GET /health/ready` 只 ping PostgreSQL，不检查 Redis/RustFS。具体实现看 [health](../internal/health/) 与 [main](../cmd/api/main.go)。RustFS 不可用不会阻止启动，不能从 API 进程存在推断媒体存储已就绪。

```bash
make monitoring
curl http://127.0.0.1:9091/metrics
make outbox-status
make reconcile
make reconcile REPLAY=1
make reconcile REBUILD=1
```

Prometheus `:9090`、Grafana `:3000` 已配置规则与看板。当前 [规则文件](../observability/prometheus/rules/tokmall.yml) 有 9 条记录规则、7 条告警；本地 Compose 未提供 Alertmanager 通知服务。traceparent 与日志关联已实现，未配置完整 OTel 导出链路。

消费者实际在 [NewConsumer](../internal/outbox/consumer.go) 默认每批 32 条、阻塞 2 秒、认领空闲阈值 30 秒、投递阈值 5；[main.go](../cmd/api/main.go) 当前只传入消费指标选项，未装配配置中的 OUTBOX_CLAIM_IDLE / OUTBOX_CONSUMER_MAX_DELIVERIES。修改这两个环境变量不能改变当前 API 消费者行为；排障以装配和构造参数为准。

直接操作 [outboxctl](../cmd/outboxctl/main.go) 的命令有 `status`、`dlq list`、`dlq requeue <event_id>`、`drain`，需提供上述配置环境变量。中继库内 DEAD 事件与消费侧 DLQ Stream 是不同失败位置；命令能处理哪个位置以其实现为准，不要把库内 requeue 当作消费 DLQ 的通用重放接口。

对账覆盖库存余额、订单库存流水、退款订单一致、订单支付一致、作者日销投影、券发行量、券使用、订单金额拆分、Outbox 投递。CLI 退出码 0=一致、1=差异、2=执行失败；`make` 可能将非零结果显示为自身失败。`--replay` 只补偿满足条件的缺失回执，不自动修复所有差异；`--rebuild` 会改写作者日销读模型，要求实际 `OUTBOX_CONSUMER_GROUP` 的所有支付/退款事件回执齐全。尚未消费的 PENDING/PUBLISHED/DEAD 都会阻止重建；未来 timeout 不阻止。事务用 NOWAIT 表锁排除并发写，争锁直接失败，退出码 2。

重建顺序：暂停相关业务写入；保持中继/消费者运行，先处理 DEAD/DLQ 或重放缺失回执，等支付/退款事件排空；再暂停会写这些表的后台任务，于维护窗口执行 `make reconcile REBUILD=1`；检查投影对账后恢复。不能只停消费者就重建，旧事件恢复后会重复累加；不能删除回执或用伪造回执绕过门禁。锁持有期间新写入会等待，大表重建需预估维护时间。`--rebuild --replay` 不会先替你排空事件，存在缺口时重建仍拒绝，必须分阶段完成。

删除恢复队列可查 `object_delete_jobs` 的 `available_at/attempts/last_error`。后台每 10 秒处理，失败延迟 1 分钟；物理删除前旧 GET URL 可能仍有效。此队列目前没有专用积压告警。

## 演练脚本的真实覆盖

脚本会造数、修改事件表或停止依赖，使用专用测试环境。它们是辅助演示，不等于每个打印的结论都有自动断言。

| 入口 | 当前限制 |
|---|---|
| `scripts/drill_coupon.sh` | 券模板、顺序领取/重复领取、用券、支付、退款退券与对账；未覆盖并发领取或关单退券 |
| `make drill` | 写入 1MiB 随机媒体后立即发布；默认媒体门禁下不可直接跑通，模拟业务需先用 MEDIA_PROBE_ENABLED=false 启动后端 |
| `make drill-failure` | Redis 停机支付段只查计数；消费者崩溃段只看 PEL；RustFS 段使用无效 key/票证，在访问存储前就被拒，未触发熔断器。需按 roadmap 修复断言与造数 |

Outbox 的 PENDING 包含未来才到期的订单 timeout 事件，恢复目标不是总 PENDING=0。观察 `available_at <= now()` 的待投递事件、过期租约、消费回执和对账差异。不能删掉一个已有投影的消费回执再重放；回执与投影同事务，人工删回执会破坏幂等依据。

## 压测

[loadgen](../cmd/loadgen/main.go) 支持 commerce/feed/penetration。`-output` 保存实际测量时长、成功请求/交易吞吐与原始延迟样本；commerce 的创建、支付、完整成功交易分别统计，`-skus` 支持均匀轮换多个SKU，`-stock` 是每个SKU的初始库存。先准备数据，关闭或明确记录限流影响，区分请求数与订单数，保存种子、提交、环境与原始指标。

```bash
RATE_LIMIT_ENABLED=false make dev-backend
make loadgen SCENARIO=commerce CONCURRENCY=50 DURATION=20s
make loadgen SCENARIO=feed CONCURRENCY=50 DURATION=20s
make loadgen SCENARIO=penetration CONCURRENCY=30 DURATION=10s
```

不把造数或压测运行在需保留的业务库；[seed_perf.sql](../scripts/seed_perf.sql) 用模拟媒体状态，不能证明真实视频校验/播放。历史结果与新版待测场景见 [性能基线](performance-baseline.md)。

[benchmark_backend.py](../scripts/benchmark_backend.py) 串行执行同版本 Feed A/B/C 和交易热点/分散SKU三轮对照，自动建立 `tokmall_bench_*` 专用数据库和独立测试桶，要求 Redis DB15 初始为空。依赖现有标准端口容器、已编译的 `bin/api`、`bin/loadgen`、`bin/reconcile`；API/指标使用18080/19091。实验结束后保留专用资源供核验，确认报告后按明确名称清理。它会清空专用库的交易表、Redis DB15，不能与其他使用DB15的程序同时运行。

```bash
python3 scripts/benchmark_backend.py --output benchmarks/2026-10-09/formal \
  --suite all --rounds 3 --feed-seconds 180 --commerce-seconds 120 --clients 10,50,100
python3 scripts/summarize_benchmark.py benchmarks/2026-10-09/formal
```

复跑新实验使用新的输出目录；同目录用于失败后继续，须保持原构建、配置和实验专用资源。汇总脚本只读取已完成轮次，轮次不足三次不能当作最终结论。中位数P99是各轮P99的中位数，不是合并样本的P99。

<a id="k6-benchmarks"></a>

### k6：最近分步实验的入口

当前分步实验用 [benchmark_k6.py](../scripts/benchmark_k6.py) 调度 [k6场景](../scripts/k6-baseline.js)：百万视频/三万用户，L1+L2，默认日志与worker，隔离实例关闭限流；交易包含视频商品来源和日销投影。脚本新建 `tokmall_bench_*` 库，并要求选定的Redis DB1～15初始为空且仅供本次实验使用。每个案例会清理该实验的交易表与Redis缓存，不能与其他程序共享该库。

在根目录准备 `bin/api`、`bin/reconcile`、`bin/k6`，确保标准端口容器可用。下面以DB12为示例，先核实该编号为专用空库；非空则选择另一专用库，不清理未知数据：

```bash
go build -o bin/api ./cmd/api
go build -o bin/reconcile ./cmd/reconcile
./bin/k6 version
BENCH_REDIS_DB=12
docker exec tokmall-redis redis-cli -n "$BENCH_REDIS_DB" DBSIZE
```

零值不等于自动获得独占使用权，应确认其他程序不使用该库。选择新的输出目录后运行，例如Feed三轮：

```bash
python3 scripts/benchmark_k6.py \
  --output .gocache/benchmarks/new-feed-run \
  --redis-db "$BENCH_REDIS_DB" \
  --suite feed --clients 100 --seconds 60 --rounds 3 \
  --settle-seconds 180
```

交易实验改为 `--suite commerce --clients 50`，会分别测1与100个SKU；新的独立实验另选专用空Redis库。`--suite all`执行默认阶梯探测，`--confirm-commerce`用于该模式的交易确认。`--resume`只用于相同Git HEAD、二进制与实验资源的续跑，不能把更新后的实现混进旧轮次。

固定VU模式是“完成一次迭代再发下一次”，不是固定到达率。Feed检查200及20项；完整交易先下单再模拟支付，两次都成功才计订单TPS。观察正常消费后核对支付、库存/流水、回执/投影和九项对账；未来timeout不算到期积压，等待条件还检查因背压延迟的待投递支付事件。压力结束后等待及对账时间不是精确消费恢复时延。

环境文件记录专用PG库名（`database`）、Redis DB、构建与二进制SHA。每轮保存k6汇总、逐请求压缩样本、指标前后快照、PG统计、进程资源与对账结果。汇总/紧凑归档：

```bash
python3 scripts/summarize_k6.py \
  .gocache/benchmarks/new-feed-run benchmarks/new-feed-run
```

源码包、构建补丁与逐请求压缩样本应另行保存，汇总脚本不替代完整原始数据备份。最近结果见 [性能证据](performance-baseline.md#latest-results)，旧版基线与诊断保留其原构建和测量条件。

<a id="lock-waits"></a>

### 动态排查数据库锁等待

压测进行时，在第二个终端连接该轮environment.json记录的实验库。下面库名是已有归档的例子，新轮次必须替换为本轮database值：

```bash
BENCH_DB=tokmall_bench_1791738182
docker exec -it tokmall-postgres psql -U tokmall -d "$BENCH_DB"
```

在psql执行并持续观察：

```sql
SELECT pid, state, wait_event_type, wait_event,
       pg_blocking_pids(pid) AS blockers,
       now() - xact_start AS transaction_age,
       left(query, 300) AS query
FROM pg_stat_activity
WHERE datname = current_database()
  AND pid <> pg_backend_pid()
  AND state <> 'idle'
ORDER BY xact_start NULLS LAST;
```

```text
\watch 0.2
```

保持默认自动提交，不把观察窗口包进长期BEGIN事务，以免复用活动快照。`Lock`与`transactionid/tuple`结合当前SQL、阻塞PID、受控SKU或参数日志解释；事务年龄不等于精确锁等待时间。阻塞者的当前/最后SQL可能已经换成后续写入，锁仍等提交才释放，需结合事务流程分析。混合流量不能仅凭SQL模板相同断言同一SKU。

连接池指标在压测实例19091，普通开发实例默认9091：

```bash
curl -s http://127.0.0.1:19091/metrics | grep '^db_pool_'
```

看in_use/idle与该轮预算，再看WaitCount/WaitDuration同一进程窗口增量。等待次数不等于超时数；累计等待是多goroutine之和，不是墙上经过时间。旧版本共享25连接，新版消费开启时前台20/消费5，需要按db标签区分。结合PG等待与Go阻塞栈定位“连接为何被占住”，不从池满直接推断应该增加连接。

代表性SQL可用 `EXPLAIN (ANALYZE, BUFFERS)` 检查计划；ANALYZE会实际执行。它是查询执行成本证据，不覆盖应用获取连接排队，也不代替运行时锁采样。历史诊断用Go观察器每200ms采集活动、约每秒采集计数，结果在 [诊断归档](../benchmarks/2026-10-11/diagnosis/README.md)。

<a id="profiles"></a>

### Go profile：在线采集与离线复习

普通API没有默认pprof端点。10月11日使用Go build overlay加入诊断文件，导入net/http/pprof、开启block/mutex抽样，并在 `127.0.0.1:19092` 启动独立HTTP监听；工具源码与overlay保存在诊断目录的diagnostic-tools.tar.gz，重建前需核对路径映射。诊断开销与正式测量分开。

只有诊断实例运行且有负载时，以下命令才有意义：

```bash
curl -o cpu.pprof 'http://127.0.0.1:19092/debug/pprof/profile?seconds=15'
curl -o block.pprof 'http://127.0.0.1:19092/debug/pprof/block?seconds=15'
curl -o mutex.pprof 'http://127.0.0.1:19092/debug/pprof/mutex?seconds=15'
curl -o goroutines.txt 'http://127.0.0.1:19092/debug/pprof/goroutine?debug=2'
```

CPU看处理器开销，block看goroutine阻塞，mutex看Go锁竞争，goroutine文本看当前栈；数据库行锁仍用PG现场说明。可以不启动压测，直接读取归档文件：

```bash
DIAG=benchmarks/2026-10-11/diagnosis/commerce-c50-trim10k-r2
go tool pprof -top -cum "$DIAG/block.pprof"
go tool pprof -top "$DIAG/profile.pprof"
```

`flat`是归属于函数本身的采样值，`cum`包含其下游调用。block中DB.conn的比例是累计goroutine阻塞时间占比，不是CPU占比或单请求延迟占比。更多profile、执行计划、Stream身份追踪与trim-proof.json见诊断归档。
