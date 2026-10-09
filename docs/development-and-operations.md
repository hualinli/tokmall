# 开发、验证与运维

> 核对日期：2026-10-07；命令与默认值对照 [Makefile](../Makefile)、[配置](../internal/config/config.go)、[CI](../.github/workflows/ci.yml)。这里说明怎么运行，业务规则与机制分别见业务文档和 ADR。

## 环境与启动

Go 版本以 [go.mod](../go.mod) 为准；Node 22、pnpm 11 与 CI 对齐。Compose 提供 PostgreSQL `localhost:5432`、Redis `localhost:6380`、MinIO `:9000`（控制台 `:9001`），服务名分别为 postgres/redis/minio。不要另建 5433 实例。

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

Docker 凭证助手异常时，按 [项目记忆](../AGENTS.md) 的 `.docker-anon/config.json` 配置绕过；手动容器命令可用 `DOCKER_CONFIG=$PWD/.docker-anon docker compose up -d postgres minio redis`。`make dev` 自身未硬编码匿名目录，需要正确的 Docker 环境。

直接启动任何 Go 工具都需要 `APP_ENV`、`JWT_SECRET`、`FEED_CURSOR_SECRET`，即使该工具只连数据库。Makefile 仅为本地开发提供密钥。生产还校验 DB/Redis/MinIO 显式配置、TLS、密钥长度与占位凭证，完整条件看 [Validate](../internal/config/config.go)。

## 媒体与旧版本升级

默认 `MEDIA_PROBE_ENABLED=true`。ffprobe/ffmpeg 缺失与坏文件不同：缺 ffprobe 记 SKIPPED；坏文件记 REJECTED；探测超时/下载暂时失败延迟重试。只有 PASSED/SKIPPED 能发布，因此正常上传后要等 worker，前端目前没有完整校验状态展示。

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

## 测试矩阵

默认 `go test ./...` 会跳过真实依赖测试，不能把默认通过描述成验证了数据库／消息／对象存储。

```bash
go test ./...
make lint          # golangci-lint 版本对照 CI 与 .golangci.yml
make verify        # fmt 会修改格式；vet/lint/test/tidy-check
scripts/preflight.sh
```

`preflight.sh` 检查格式、vet、测试、tidy 漂移，依赖安装后检查前端类型；它不执行 lint、前端生成物漂移检查或 E2E，不能替代完整 CI。

```bash
docker compose up -d postgres redis minio
TOKMALL_RUN_PG_INTEGRATION=1 \
TOKMALL_RUN_REDIS_INTEGRATION=1 \
TOKMALL_RUN_MINIO_INTEGRATION=1 \
TOKMALL_TEST_DATABASE_DSN='postgres://tokmall:tokmall@localhost:5432/tokmall?sslmode=disable' \
TOKMALL_TEST_REDIS_ADDR=localhost:6380 \
go test ./... -count=1
```

PG 测试用隔离 schema；Redis 测试用自己的 key/stream，不要在共享实例盲目 FLUSHDB。MinIO 测试开关目前仅覆盖 [上传封存集成测试](../internal/storage/upload_integration_test.go)，使用固定本地端点/开发凭证和临时 bucket，并清理自身对象。各测试的依赖条件以测试源文件为准。

并发检查示例：`go test -race ./internal/order ./internal/video ./internal/feed ./internal/outbox ./internal/user`。启用上述环境变量后也会运行相应真实依赖分支；全量 race 不是每次改文档都需要运行。

受限沙箱可将 Go 缓存放仓库内：

```bash
export GOPATH="$PWD/.gopath" GOMODCACHE="$PWD/.gopath/pkg/mod"
export GOCACHE="$PWD/.gocache" GOSUMDB=off
```

前端生成、类型、单测与 E2E 看 [web README](../web/README.md)。[CI](../.github/workflows/ci.yml) 有 backend/contract/web/e2e 四个 job，PG/Redis 服务显式开启测试；真实 MinIO 封存测试目前没有在 backend job 开启，E2E 使用另起的 MinIO 且关闭媒体探测。[Security](../.github/workflows/security.yml) 独立执行 CodeQL/govulncheck。CI 跳过纯 Markdown 改动，文档链接需要单独核验。

## 观测与恢复

`GET /health/live` 恒返回 OK；`GET /health/ready` 只 ping PostgreSQL，不检查 Redis/MinIO。具体实现看 [health](../internal/health/) 与 [main](../cmd/api/main.go)。MinIO 不可用不会阻止启动，不能从 API 进程存在推断媒体存储已就绪。

```bash
make monitoring
curl http://127.0.0.1:9091/metrics
make outbox-status
make reconcile
make reconcile REPLAY=1
make reconcile REBUILD=1
```

Prometheus `:9090`、Grafana `:3000` 已配置规则与看板。当前 [规则文件](../observability/prometheus/rules/tokmall.yml) 有 9 条记录规则、7 条告警；本地 Compose 未提供 Alertmanager 通知服务。traceparent 与日志关联已实现，未配置完整 OTel 导出链路。

直接操作 [outboxctl](../cmd/outboxctl/main.go) 的命令有 `status`、`dlq list`、`dlq requeue <event_id>`、`drain`，需提供上述配置环境变量。中继库内 DEAD 事件与消费侧 DLQ Stream 是不同失败位置；命令能处理哪个位置以其实现为准，不要把库内 requeue 当作消费 DLQ 的通用重放接口。

对账覆盖库存余额、订单库存流水、退款订单一致、订单支付一致、作者日销投影、券发行量、券使用、订单金额拆分、Outbox 投递。CLI 退出码 0=一致、1=差异、2=执行失败；`make` 可能将非零结果显示为自身失败。`--replay` 只补偿满足条件的缺失回执，不自动修复所有差异；`--rebuild` 会改写作者日销读模型，应在明确维护范围内运行。

删除恢复队列可查 `object_delete_jobs` 的 `available_at/attempts/last_error`。后台每 10 秒处理，失败延迟 1 分钟；物理删除前旧 GET URL 可能仍有效。此队列目前没有专用积压告警。

## 演练脚本的真实覆盖

脚本会造数、修改事件表或停止依赖，使用专用测试环境。它们是辅助演示，不等于每个打印的结论都有自动断言。

| 入口 | 当前限制 |
|---|---|
| `scripts/drill_coupon.sh` | 券模板、顺序领取/重复领取、用券、支付、退款退券与对账；未覆盖并发领取或关单退券 |
| `make drill` | 写入 1MiB 随机媒体后立即发布；默认媒体门禁下不可直接跑通，模拟业务需先用 MEDIA_PROBE_ENABLED=false 启动后端 |
| `make drill-failure` | Redis 停机支付段只查计数；消费者崩溃段只看 PEL；MinIO 段使用无效 key/票证，在访问存储前就被拒，未触发熔断器。需按 roadmap 修复断言与造数 |

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
