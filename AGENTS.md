# Project memory

## 项目简介

TokMall：基于 CloudWeGo 技术栈（Hertz）的短视频 Feed + 商城一体化后端（学习项目）。
目标不是一次实现 TikTok，而是通过可演进的系统掌握生产级后端能力：分层架构、状态机、
幂等、游标分页、缓存、观测性。目前已带 React 前端（web/，OpenAPI 契约驱动，含 e2e）。

当前形态：**模块化单体**。模块间不共享类型（只读共享表时用各自的 read model / 直接 SQL），
未来拆服务时按模块边界拆。

## 技术栈

- HTTP：CloudWeGo Hertz v0.10（含 hertz-contrib requestid / monitor-prometheus / logger-slog）
- ORM：GORM + PostgreSQL（golang-migrate 管理 schema，禁止 AutoMigrate）
- 对象存储：MinIO（预签名 PUT 直传 + 预签名 GET 播放 URL）
- 认证：JWT（access 15min / refresh 7d，HS256）+ Redis 黑名单（refresh 可撤销，ADR-011）
- 缓存：Redis（6380），cache-aside + singleflight + double-check（ADR-012）
- 观测：slog 结构化日志（stdout + lumberjack 轮转文件）、Prometheus /metrics（默认只监听 127.0.0.1:9091）
- 事件：Transactional Outbox + Redis Streams（消费组/XACK/XAUTOCLAIM）+ DLQ 与人工重放（ADR-014）
- 对账：六项可执行检查 + 对账驱动重放 + 读模型重建（cmd/reconcile）
- 契约：OpenAPI 3（internal/api/openapi.yaml）：route-sync（路由级）+ spec_test（schema ↔ DTO 字段/类型/必填）
- 测试：后端 testify（require）+ hertz ut 内存链路 + 显式开启的真实 PG/Redis 集成测试（internal/testsupport 提供隔离 schema）；前端 Playwright e2e + Vitest 单测
- 前端：React + TanStack Router + React Query + Vite + Tailwind（web/，openapi-typescript 生成类型）

## 目录结构

```text
cmd/api/main.go          服务入口：装配依赖 + 路由分组（公开组 /api/v1、受保护组）
internal/
  api/                   OpenAPI 契约（spec.go embed openapi.yaml + route-sync 测试）
  config/                环境变量配置（默认值 + required）
  database/              连接 + 连接池(25/5/1h) + migrate
  cache/                 Redis 客户端（模块不持有原始 client，main 装配注入消费侧接口）
  storage/               MinIO 封装（预签名 PUT 直传、预签名 GET 播放 URL、StatObject 翻译 NoSuchKey）
  token/                 JWT Manager + GenClaims + Verify（access/refresh 类型区分）
  middleware/            auth（Bearer 校验 + user_id/role 注入，AuthOptional 用于公开接口）、recovery、requestid
  response/              BizError 统一错误码 + HTTP 状态映射 + Abort
  health/                存活/就绪探针
  user/                  注册/登录/me + refresh 旋转 + logout 黑名单（model + repository + service + handler）
  video/                 发布/详情/发布/删除 + upload_ticket 防伪造（状态机）+ 作者视频列表 + 播放事件
  interaction/           点赞/关注（幂等，ON CONFLICT DO NOTHING）+ 关注/粉丝列表
  feed/                  Feed 游标分页（HMAC 签名游标）+ 首页缓存 + 关注 Feed（拉模式）+ Ranker 排序 + read model
  product/               商品/SKU、上下架与商品列表/详情
  inventory/             库存与幂等流水
  order/                 单 SKU 订单、模拟支付、取消/超时关单(lazy expiry)、买卖双方查询与履约、
                         售后全额退款（申请/裁决/库存回补策略，状态机含 REFUND_REQUESTED/REFUNDED）
  promotion/             已发布视频挂本人在售商品
  attribution/           作者成交统计（实时聚合）+ 事件驱动的作者日销投影（projector.go）
  outbox/                Transactional Outbox：仓储(SKIP LOCKED+租约)、中继(退避/DLQ)、
                         传输抽象(Redis Streams)、消费组消费者(回执+投影同事务)、重放
  reconcile/             六项对账检查 + 对账驱动重放 + 读模型重建
  breaker/               三态熔断器（closed/open/half-open + 指数退避 + 失败分类）+ 存储装饰器
  coupon/                优惠券域：满减券模板/领券（锁行+条件更新防超发）/我的券/规则纯函数
  existence/             缓存穿透防护：自研布隆过滤器（双缓冲轮换）+ Redis 负缓存 + 重建任务
  tracing/               W3C traceparent 解析/生成/透传（零依赖，可平滑升级到 OTel）
  testsupport/           跨模块集成测试脚手架（隔离 schema + 迁移 + Redis 开关）
  observability/         db_pool / feed_cache / outbox / reconcile / existence / http 指标、
                         ContextHandler（日志自动注入 request_id/trace_id/route + 脱敏）
migrations/             版本化 SQL 迁移（000001～000019；商域从 000007 起，退款在 000015，
                         不变量加固在 000016，Outbox 与投影在 000017，users.status 在 000018，
                         优惠券与订单金额拆分在 000019）
cmd/outboxctl/          Outbox 运维命令：status / dlq list / dlq requeue / drain
cmd/reconcile/          对账命令：--json / --replay / --rebuild，退出码 0=一致 1=有差异 2=失败
cmd/loadgen/            压测客户端：commerce / feed / penetration（缓存穿透）三个场景
web/                    React 前端（TanStack Router + React Query + Vite + Playwright e2e）
observability/          Prometheus 抓取配置与记录/告警规则、Grafana provisioning 与看板 JSON
scripts/                seed_perf.sql 造数、drill_commerce_loop.sh 端到端演练、
                        drill_failure_modes.sh 故障演练、drill_coupon.sh 券闭环演练
.github/workflows/      CI：后端/契约/前端/e2e 四个 job；security.yml：CodeQL + govulncheck
docs/                   当前架构/业务文档 + Feed/Mall 演进计划 + ADR + 性能基线（git 跟踪）
```

## 里程碑状态

| 里程碑 | 状态 | 要点 |
|---|---|---|
| M0 工程骨架 | ✅ | Hertz + PG + 配置 + 中间件 + 日志 |
| M1 视频发布 | ✅ | MinIO 预签名 + upload_ticket + 状态机 |
| M2 Feed 分页 | ✅ | 游标 (published_at,id) + HMAC + 部分索引 |
| M3 互动幂等 | ✅ | 唯一约束 + ON CONFLICT + 并发验证 |
| L2 观测性 | ✅ | /metrics(9091) + db_pool + 基线压测 |
| M4 缓存 | ✅ | Redis(6380) + singleflight + 首页缓存：1M 数据 6.6k→11.3k QPS |
| M4+ Auth 闭环 | ✅ | /refresh 旋转 + /logout 黑名单（ADR-011，Redis 引入的附带收益） |
| M4+ 内容域扩展 | ✅ | 预签名 GET 播放 URL（route A）、作者视频列表 + 硬删 MinIO、公共主页 + 社交列表 + 点赞数 |
| M4+ 契约与前端 | ✅ | OpenAPI 契约 + route-sync 测试 + React 前端（TanStack Router）+ Playwright e2e |
| M5 播放事件（同步版） | ✅ | play_events 表 + POST /videos/{id}/play：event_id 幂等（UUID v7）、仅 PUBLISHED、未来时间拒绝；Kafka 异步化待做 |
| M6 关注 Feed | ✅ | 当前为拉模式完整时间线：先查关注数，少量关注作者用有界归并，多关注作者用全局时间索引；原每作者 TOP 5 为历史实现；HotRanker 仅保留为包内排序实验 |
| M8 排序演进 | ✅（边界已收口） | Ranker 接口 + TimeRanker 生产游标分页；HotRanker 仅单元实验（排序键未与游标闭合，配置 fail-fast 禁止启用）；HybridRanker 待做 |
| M7 限流 | ✅ | Redis 固定窗口（lua INCR+EXPIRE）+ 三档规则（auth 10/min/IP、read 200/10s/IP、write 100/10s/user）+ fail-open 降级 + RATE_LIMITED 429 |
| Commerce 阶段 1～3 | ✅ | 商品/SKU/库存、下单/支付/关单、视频挂商品与卡片、作者成交统计；`ab9d062` |
| Commerce v1 订单收尾 | ✅ | 买家/商家订单查询、模拟发货与确认完成、前端订单页；`b5349ea` |
| 安全与正确性收口 | ✅ | 审计 P0/P1 共 10 项：限流绕过(XFF)、超时订单可支付(lazy expiry)、并发 refresh 双签发(Claim)、singleflight 故障放大、输入校验与错误语义；`ab8c92a` |
| 售后（退款）闭环 | ✅ | 全额退款申请/裁决、未发货回补/已发货不回补、部分唯一索引、履约与退款状态图分离；`6e1dd1e` |
| Transactional Outbox | ✅ | 事件同事务、至少一次投递、幂等消费、DLQ 重放、作者日销投影、`outboxctl`；`ff288ac` |
| 对账与可证明性 | ✅ | 六项检查、对账驱动重放、读模型重建、契约 schema 校验、CI 四 job、压测与故障演练；`1436fd5` `68192ea` |
| M5-Kafka | 搁置 | play 事件异步化（Kafka + Worker）：已压测证明当前同步写不是读路径瓶颈；若未来写吞吐成为瓶颈再引入 |
| Outbox + Kafka | 部分 | Outbox 与可靠投递已完成（默认 Redis Streams，见 ADR-014）；Kafka 仅保留为可替换的 Publisher 实现，未接入（与已搁置的播放事件异步化是不同决策） |
| 缓存穿透防护 | ✅ | 自研布隆过滤器（双缓冲轮换 + warmup 保护）+ Redis 负缓存（TTL 抖动）+ 重建任务；实测不存在的 ID 从 2,996→5,086 req/s 且数据库零查询；ADR-022 |
| 超时关单双触发 | ✅ | Outbox `available_at` 实现延迟事件（加速器）+ 轮询 Worker（安全网），数据库为唯一事实源；ADR-021 |
| 优惠券域（v1 满减券） | ✅ | 发行量+每人限领+领取截止+券状态机+退款退券+三项对账；100 并发抢 10 张恰好发 10 张；ADR-024 |
| 依赖级熔断 | ✅ | 自研三态熔断器（装饰器注入对象存储）：MinIO 停机时 3~5s 超时 → 熔断后 0.7ms 快速失败 + 503；失败分类排除"对象不存在"；ADR-023 |
| 请求级可观测性 | ✅ | ContextHandler 自动注入 request_id/trace_id/route + 脱敏；自研 W3C traceparent；路由模板化耗时直方图；访问日志 |
| 观测栈与告警 | ✅ | 8 条记录规则（`level:metric:operations`）+ 6 条告警 + 11 面板看板（JSON 入库）；测试保证规则引用的指标真实存在（变异验证过） |
| 工程门禁增强 | ✅ | CI 加 fmt/tidy 漂移检查、最小权限、并发取消、路径过滤；新增 CodeQL + govulncheck；`make verify/coverage/vulncheck/help`；`scripts/preflight.sh` |

> 注：M4+ 三行不在原里程碑规划内（按"出现可测量问题再引入技术"的原则，这些是 M4 的
> 自然延伸：Redis 引入后顺手补 auth 可撤销性，内容域扩展是前端联调需求驱动）。
> M5/M6/M8/M7 为方向 1 + 治理交付。本轮（2026-10）新增的四行来自一次完整审计 +
> 售后/Outbox/对账演进，面试讲解入口是 `docs/interview-walkthrough.md`。
> Commerce v1 的当前范围与实现见
> `docs/commerce-business.md`、`docs/commerce-v1-technical-design.md`；后续工程化方向见
> `docs/interview-production-readiness-plan.md`。文档入口和历史设计说明见 `docs/README.md`。

## 关键设计约定（改代码前必读）

- **错误三层链**：repo 翻译 ORM 错误为领域哨兵 → service 判定分类+日志分级（业务失败不打 Error）→ handler 映射 BizError，未知一律 500。接口定义在消费侧。
- **状态机**：video 模块表驱动 `statusTransitions` + DB 条件更新（`WHERE status=from`）防并发。
- **可见性**：DELETED 对所有人 404，未发布仅作者可见，不可见一律 ErrNotFound（不泄露存在性）。
- **幂等**：关系表唯一约束 + `ON CONFLICT DO NOTHING`，结果语义固定（like→true）。
- **游标**：表示上一页最后一条（非当前时间），防翻页重复；rank_version 失效旧游标。只有排序键与游标一致的排序器才能上线；当前生产关注 Feed 仅支持 time-v1，HotRanker 不能通过配置或 Service 启用。
- **分层数据流**：handler(HTTP DTO) → service(领域 Params) → repo(model)，model 永不出 service（PasswordHash json:"-" 级别防护）。
- **列名/状态类型化**：`column`/`Status` 自定义类型防拼错；到 GORM 边界必须显式转 string。
- **连接池**：max_open=25, max_idle=5, lifetime=1h（M3 基线接近饱和；M4 缓存命中后 db_pool_wait_count 归零，瓶颈已移到 Redis 读，见 performance-baseline.md）
- **售后状态**：订单主状态含 REFUND_REQUESTED/REFUNDED，`refund_from_status` 记录申请前状态；
  履约命令走独立的 `fulfillmentTransitions`（**不得**复用完整状态图，否则能绕过退款裁决发货）
- **退款库存策略**：未发货退款回补（`ORDER_REFUNDED` 流水，(sku_id,reason,order_no) 唯一）；已发货退款不回补
- **事件**：业务事实与 outbox_events 必须同一事务写入（`outbox.Enqueue*` 只接受 tx 句柄）；
  投递至少一次；消费侧"回执 + 投影"同事务，**没有回执 ⟺ 投影没生效**（对账重放依据）
- **对账口径**：毛额（order_count/units/gmv）记曾支付过的全部订单，退款按发生日单独归集；
  改投影口径必须同步改 reconcile 检查，否则对账本身成为噪音源
- **签名密钥**：JWT_SECRET / FEED_CURSOR_SECRET 在所有环境必填（无内置默认值）；
  TRUSTED_PROXIES 默认空 = 不信任任何转发头（不要配 0.0.0.0/0）
- **超时关单双触发**：下单事务写 `commerce.order.timeout.v1`（available_at = expires_at）
  作为加速器，轮询 Worker 是安全网；**数据库是唯一事实源**，消息只负责"更早发生"（ADR-021）
- **缓存穿透防护**：布隆过滤器说"不存在"才直接 404；只对"记录确实不存在"写负缓存，
  可见性拒绝（未发布视频对非作者）**不能写**（否则作者在 TTL 内看不到自己的视频）（ADR-022）
- **日志关联**：一律用 `slog.*Context(ctx, ...)`；ContextHandler 自动附加
  request_id/trace_id/span_id/route/user_id，并按字段名黑名单脱敏。
  指标标签只用路由模板（`/api/v1/videos/:id`），禁止 user_id/order_no 等高基数字段
- **GORM 日志**：ParameterizedQueries + IgnoreRecordNotFoundError + SlowThreshold=200ms
- **券**：规则只有一份实现（`coupon.DiscountFor`，门槛按整单金额）；领取走
  "锁券行 → 条件更新发行量 → 计数校验限领"三层；订单金额恒满足
  `total = original - discount`（DB CHECK 兜底）；退款成功退券（过期则标 EXPIRED）；
  对账覆盖发行量守恒、券-订单一致、金额拆分
- **告警规则**：改 `observability/prometheus/rules/*.yml` 后必须跑
  `go test ./internal/observability/`——规则里引用的指标名会被校验，写错即失败；
  记录规则命名遵循 `level:metric:operations`；只对"已到期"的 Outbox 事件告警
- **熔断**：依赖级熔断器用装饰器注入（业务代码零改动）；`Config.IsFailure` 必须排除
  "业务正常答复"（如对象不存在、redis.Nil），否则正常 404 会打开熔断器；
  熔断打开对外返回 **503**（可重试）而不是 500；对象存储不可用不阻塞服务启动

## 常用命令

```bash
make dev                                    # 一键：依赖容器 + 后端 + 前端（全部后台，日志 logs/）
make stop                                   # 停前后端（幂等）
make dev-backend / make dev-web             # 只起后端 / 只起前端（JWT_SECRET 等已内置，可 make dev JWT_SECRET=xxx 覆盖）
tail -f logs/api.log logs/web.log           # 看日志
go test ./...                               # 后端全量测试
cd web && pnpm test:e2e                     # Playwright e2e（需后端 + 依赖容器在跑）
curl localhost:9091/metrics                 # 指标
make drill                                  # 端到端业务闭环演练（curl+psql 全程可见）
make drill-failure                          # 故障演练（Redis 停机 / 事件丢失 / 对账重放 / 熔断降级）
scripts/drill_coupon.sh                     # 优惠券闭环演练（发券→领券→用券→退款退券→对账）
make reconcile [REPLAY=1] [REBUILD=1]       # 对账 / 重放 / 重建读模型
make outbox-status                          # Outbox 积压与死信
make loadgen CONCURRENCY=50 DURATION=20s    # 下单链路压测（限流需关闭）
./bin/loadgen -scenario penetration -concurrency 30 -duration 10s   # 缓存穿透压测
make verify                                 # 本地一次跑完 CI 门禁（fmt/vet/test/tidy-check）
make monitoring                             # 观测栈：Prometheus(:9090) + Grafana(:3000)，规则与看板已 provision
make coverage / make vulncheck / make help   # 覆盖率 / 漏洞扫描 / 目标说明
scripts/preflight.sh                        # 提交前自检（含前端 tsc）
wrk -t4 -c50 -d10s http://localhost:8080/api/v1/feed   # Feed 压测（基线见 docs/performance-baseline.md）
```

## 环境陷阱

- **docker compose 报 `error getting credentials`**：本机 Docker 凭证助手异常。
  用匿名配置绕过：`DOCKER_CONFIG=$PWD/.docker-anon docker compose up -d`，
  且 `.docker-anon/config.json` 需要 `{"cliPluginsExtraDirs": ["/Users/<you>/.docker/cli-plugins"]}`
  才能找到 compose 插件（该目录已 gitignore）。Redis 镜像固定为 `redis:7.4-alpine`
  （本地已有该 tag，避免触发拉取）。

- PostgreSQL 标准端口 5432（docker-compose 已映射）；本机 6379 被 go-oj 占用，Redis 走 6380（compose 已映射）。
  **不要额外创建 5433 之类的 PG 实例**：早前文档里的 5433 e2e 实例是 5432 被临时占用时手动 `docker run` 的权宜之物，非标准配置，已清理。
- 重启服务：`make stop && make dev`（后端即 bin/api 进程；**不要用 `lsof -ti :8080 | xargs kill -9`**——本机
  Shadowrocket/微信也监听 8080，会一起误杀；`make stop` 只匹配 bin/api 和 vite 进程）。
  go run 子进程名会变，压测一律用 `make build` 的 `./bin/api` 起服务。
- macOS 无 `setsid`，后台起服务用 `nohup ... & disown`，且 E2E 尽量在同一命令内完成。
- `docs/`、`AGENTS.md` 由 git 跟踪（早前误写为 gitignore，已修正）：AGENTS.md 是项目记忆、
  docs/ 收录当前实现、演进计划、ADR 和压测基线；入口见 `docs/README.md`。
- **受限沙箱（如 AI 代理）里跑 Go**：默认 GOCACHE/GOMODCACHE 在 workspace 外不可写，
  需要 `GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gopath/pkg/mod GOCACHE=$PWD/.gocache GOSUMDB=off`
  （`.gopath/`、`.gocache/` 已 gitignore）。
- 重启后端后如果 `/metrics` 无响应：端口可能仍在 TIME_WAIT。服务启动后会主动探测
  `/metrics` 并在不可达时打 ERROR（`metrics endpoint is not reachable`），
  看到这条日志说明观测性已降级，重启即可。

## 工作方式（Mentor 模式）

本项目由一个资深后端导师（AI）指导开发。指导原则：

- 少写具体代码，多教思路：分层、错误处理、并发、性能、安全等生产级知识。
- 先给计划 → 用户审核 → 按步骤执行，每步完成都要解释"为什么这样写"。
- 用户写代码后负责 review，按 P0（崩溃/安全）→ P1（正确性）→ P2（质量）排序指出问题。
- 若用户设计中存在"学生思维/玩具项目思维"的严重缺陷（分布式事务、网络安全、性能瓶颈、
  认证绕过等盲区），**主动中断并发红色警告**，给出工业级解法。
- 用户确实写不出来时才直接给代码，且要给可抄的完整实现。
- 避免"顺从性陷阱"：不为了附和用户而同意错误设计，每个里程碑的简化决策要显式记录（ADR）。
