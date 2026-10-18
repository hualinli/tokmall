# 开发与运维

本文回答"怎么运行、怎么测试、出问题怎么查和恢复"。命令以 [Makefile](../Makefile) 为准，配置默认值以 [config.go](../internal/config/config.go) 为准，CI 见 [ci.yml](../.github/workflows/ci.yml)。压测操作见 [性能](performance.md#how-to-run)。

## 环境与启动

需要 Go（版本见 go.mod）、Node 22、pnpm 11、Docker/Compose；真实媒体校验还需要 ffprobe/ffmpeg。

```bash
cd web && pnpm install --frozen-lockfile && cd ..
make dev     # 起 PG/Redis/RustFS，编译并启动后端与前端
make logs    # 或看 logs/api.log、logs/web.log
make stop    # 强制停止 bin/api 与 Vite，数据容器保留
```

| 入口 | 地址 |
|---|---|
| 网页 | http://localhost:5173 |
| API | http://localhost:8080/api/v1 |
| OpenAPI | http://localhost:8080/api/v1/docs/openapi.yaml |
| 指标 | http://127.0.0.1:9091/metrics |
| RustFS 控制台 | http://localhost:9001 |

**端口与数据。** 依赖用标准端口：PG 5432、Redis 6379、RustFS 9000/9001，都只绑定 127.0.0.1，不要另起 5433 实例。PG 和 Redis 使用命名卷 `tokmall_pg-data`、`tokmall_redis-data`，重建容器不丢数据；`docker compose down -v` 会清空全部本地数据（包括 `tokmall_rustfs-data`）。改了 Compose 里的端口绑定，需要重建对应容器才会生效。

**停机。**
- `make stop` 是强制停止，不能用来验证优雅停机；要验证，就给明确的后端 PID 发 SIGTERM，再看 worker 退出日志。
- 不要按 8080 端口批量 kill，可能误杀本机其他应用。
- macOS 没有 setsid，后台运行用 nohup。

**Docker 凭证助手异常。** 用 `DOCKER_CONFIG=$PWD/.docker-anon docker compose up -d postgres redis rustfs`；其中 `cliPluginsExtraDirs` 需要指向本机的 Docker 插件目录。

**直接运行 Go 工具。** 必须提供 `APP_ENV`、`JWT_SECRET`、`FEED_CURSOR_SECRET`，即使工具只连数据库。Makefile 里的密钥仅供本地开发。

**受限沙箱的 Go 缓存：**

```bash
export GOPATH="$PWD/.gopath" GOMODCACHE="$PWD/.gopath/pkg/mod"
export GOCACHE="$PWD/.gocache" GOSUMDB=off
```

**对象存储。** 默认使用 `rustfs/rustfs:1.0.1`。应用读通用的 `S3_*` 变量，客户端库是 minio-go。旧的 `MINIO_*` 变量需要改名；旧 `minio-data` 卷不会迁移。部署到其他域名时，要显式设置 `RUSTFS_CORS_ALLOWED_ORIGINS`。

## 媒体校验开关

默认 `MEDIA_PROBE_ENABLED=true`。缺 ffprobe 时记 SKIPPED，坏文件记 REJECTED，暂时失败延迟重试。现有 E2E 和演练脚本使用合成媒体，需要关闭探测：

```bash
MEDIA_PROBE_ENABLED=false make dev-backend
```

关闭探测的模式不能用来证明媒体准入能力。

## 自动确认收货

| 变量 | 默认 | 含义 |
|---|---|---|
| `ORDER_AUTO_COMPLETE_AFTER` | `168h` | 发货到自动确认的窗口 |
| `ORDER_AUTO_COMPLETE_REJECT_GRACE` | `72h` | 拒绝退款退回待收货后，至少还剩的时间 |
| `ORDER_AUTO_COMPLETE_ENABLED` | `true` | 是否启动扫描 worker |
| `ORDER_AUTO_COMPLETE_INTERVAL` / `_BATCH_SIZE` | `1m` / `100` | 扫描周期与每批条数 |

关闭 worker 只是停止扫描，发货和拒绝退款仍会写入截止时间。演示时可以用 `ORDER_AUTO_COMPLETE_AFTER=2m make dev-backend`，发货两三分钟后就能看到订单被自动确认。窗口只影响之后发货的订单。

## 退款自动同意

| 变量 | 默认 | 含义 |
|---|---|---|
| `REFUND_AUTO_APPROVE_AFTER` | `48h` | 申请到商家处理截止的窗口，写入 `respond_by` |
| `REFUND_AUTO_APPROVE_ENABLED` | `true` | 是否启动扫描 worker |
| `REFUND_AUTO_APPROVE_INTERVAL` / `_BATCH_SIZE` | `1m` / `100` | 扫描周期与每轮最多处理笔数 |

关闭 worker 只是停止扫描，新的退款申请仍会写入截止时间。窗口只影响之后的申请。每轮最多处理 `BATCH_SIZE` 笔，每笔单独一个事务。

## 迁移与升级

API 启动时由 [migrate](../internal/database/migrate.go) 执行版本化 SQL。仓库没有完整的 up/down 兼容性流水线，回滚迁移也不等于恢复业务状态，回滚前先确认备份。

| 迁移 | 升级/回滚注意 |
|---|---|
| 000021 | 退款后券可重用。已有重用历史时，无法恢复旧的唯一约束 |
| 000022 | 旧的 PENDING + PUBLISHED 视频回到 PENDING_REVIEW，并加上发布门禁 |
| 000023 | 为历史 DELETED 视频回补清理任务；旧的未核销上传票证失效。回滚前必须停写，并等 `object_delete_jobs` 清空，因为 down 会直接删掉这张表 |
| 000024 | 释放历史取消/过期订单占用的券；券被重用后，回滚可能被拒绝 |
| 000025 | 新增 `auto_complete_at`，存量已发货订单按 `fulfilling_at + 7 天` 回填。升级后，已发货超过 7 天的订单会在第一次扫描时被自动确认；不想这样的话，先调整回填条件再升级 |
| 000026 | 退款单增加 `respond_by` 和 `decision_source`。已裁决记录按 `requested_at + 2 天` 回填，来源回填为 MERCHANT；仍在申请中的退款从升级时刻起再给 2 天，避免一上线就被自动同意。这 2 天与 `DefaultRefundRespondWindow` 相同。已有 TIMEOUT 裁决时 down 会拒绝 |

**升级旧部署。** 先停旧进程，等旧 PUT URL 的最长有效期（默认 15 分钟）过去，再启动新版本。滚动发布无法撤销已签发的签名。

## 测试

```bash
go test ./...          # 默认跳过真实依赖测试
make lint              # 提交前必跑
make verify            # fmt / vet / lint / test / tidy-check
scripts/preflight.sh   # fmt / vet / test / tidy + 前端类型；不含 lint 和 E2E
```

**真实依赖测试**要显式开启，默认 `go test` 通过不代表验证了数据库、Redis 或对象存储：

```bash
docker compose up -d postgres redis rustfs
TOKMALL_RUN_PG_INTEGRATION=1 \
TOKMALL_RUN_REDIS_INTEGRATION=1 \
TOKMALL_RUN_S3_INTEGRATION=1 \
TOKMALL_TEST_DATABASE_DSN='postgres://tokmall:tokmall@localhost:5432/tokmall?sslmode=disable' \
TOKMALL_TEST_REDIS_ADDR=localhost:6379 \
go test ./... -count=1
```

- **PG 测试**使用隔离 schema。
- **Redis 测试**多数用各自的 key 和 stream；[热榜测试](../internal/feed/hot_cache_test.go) 固定用 DB9，前后都会 FlushDB，所以 DB9 不能存要保留的数据。
- **S3 测试**每次创建临时 bucket，覆盖签名长度/类型、ETag 封存、防覆盖、Range、CORS、签名到期、幂等删除。

**其他检查：**
- 竞态检测：`go test -race ./internal/order ./internal/video ./internal/feed ./internal/outbox ./internal/user`。
- 前端测试见 [web README](../web/README.md)。
- 改了 Prometheus 规则后跑 `go test ./internal/observability/`。

CI 有 backend、contract、web、e2e 四个 job，都显式开启了真实依赖；另有独立的 Security workflow（CodeQL、govulncheck）。纯 Markdown 改动会跳过 CI，文档链接需要自己检查。

## 观测

```bash
make monitoring                  # Prometheus :9090、Grafana :3000
curl http://127.0.0.1:9091/metrics
make outbox-status
```

- `/health/live` 恒返回 OK；`/health/ready` 只检查 PG。RustFS 不可用不阻止启动，所以 API 进程在运行不代表存储可用。
- 启动日志出现 `metrics endpoint is not reachable` 时，检查监听地址和日志。
- 规则文件有 9 条记录规则、7 条告警，本地没有 Alertmanager。
- `object_delete_jobs` 积压还没有专用告警，可以查 `available_at/attempts/last_error`。
- Redis 慢命令日志只记录命令、耗时和 key 前缀，XREAD 这类阻塞读不计入，见 [slowlog.go](../internal/cache/slowlog.go)。

**Outbox 积压判断。** PENDING 里包含未来才到期的订单超时事件，所以恢复目标不是"PENDING 总数为 0"。要看 `available_at <= now()` 的待投递事件、过期租约、消费回执和对账差异。另外，Redis 消费组 `pending=0、lag=0` 也不代表业务处理完了。

**消费者参数。** [NewConsumer](../internal/outbox/consumer.go) 默认每批 32 条、阻塞 2 秒、认领空闲阈值 30 秒、最多投递 5 次。`main.go` 目前没有把 `OUTBOX_CLAIM_IDLE` / `OUTBOX_CONSUMER_MAX_DELIVERIES` 传给消费者，所以改这两个环境变量不生效。

## 对账与恢复

```bash
make reconcile            # 九项检查；退出码 0=一致，1=有差异，2=执行失败
make reconcile REPLAY=1   # 以原 event_id 重放缺回执的已发布事件
make reconcile REBUILD=1  # 从事实重建作者日销投影
```

[outboxctl](../cmd/outboxctl/main.go) 支持 `status`、`dlq list`、`dlq requeue <event_id>`、`drain`。注意 PG 内的 DEAD 事件和消费侧的 DLQ Stream 是两个位置，处理前先确认命令作用于哪一个。

**重建投影的步骤**（机制见 [架构](architecture.md#recovery)）：

1. 暂停相关业务写入。
2. 保持中继和消费者运行，处理 DEAD/DLQ、重放缺失回执，等支付和退款事件排空。
3. 暂停会写这些表的后台任务，在维护窗口执行 `make reconcile REBUILD=1`。
4. 检查投影对账结果，然后恢复写入。

几种错误做法：
- 只停消费者就重建：旧事件恢复后会重复累加。
- 删除或伪造回执：回执和投影同事务，删回执会破坏幂等依据。
- 合并成 `--rebuild --replay` 一步执行：它不会先替你排空事件，有缺口时重建照样会被拒绝。

## 演练脚本

脚本会造数、修改事件表或停止依赖，只能在专用测试环境运行。它们打印的结论并不都有自动断言。

| 入口 | 已知限制 |
|---|---|
| `scripts/drill_coupon.sh` | 覆盖顺序领券、用券、支付、退款退券、对账；没有覆盖并发领券和关单退券 |
| `make drill` | 用 1MiB 随机数据当媒体并立即发布，需要先以 `MEDIA_PROBE_ENABLED=false` 启动后端 |
| `make drill-failure` | Redis 停机段只查计数；消费者崩溃段只看 PEL；RustFS 段在访问存储前就被拒绝，没有触发熔断器 |
