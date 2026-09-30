# TokMall

基于 CloudWeGo Hertz 的**短视频 Feed + 商城一体化后端**（模块化单体），
用 PostgreSQL、Redis、MinIO 支撑一条完整业务链：

```text
上传发布视频 → 视频挂本人在售商品 → Feed 浏览 → 下单(防超卖) → 模拟支付
    → 商家发货 → 买家确认收货 → 买家申请退款 / 商家裁决(库存回补策略)
    → 支付与退款事件写入 Transactional Outbox → 至少一次投递 + 幂等消费
    → 作者日销投影 → 六项对账检查(可重放、可重建读模型)
```

`web/` 是配套的 React 前端（TanStack Router + React Query + Playwright e2e）。

**这个项目的重点不是功能数量，而是每个结论都有证据**：
防超卖有 500 并发实验，幂等有并发重放实验，事件不丢有故障演练，
数据一致有对账任务，性能有压测基线，安全有审计与回归测试。

## 快速开始

```bash
make dev            # 依赖容器 + 后端 + 前端（后台运行，日志在 logs/）
# 前端 http://localhost:5173
# API  http://localhost:8080   （契约 http://localhost:8080/api/v1/docs/openapi.yaml）
# 指标 http://localhost:9091/metrics
make stop           # 停前后端
```

```bash
go test ./...                              # 后端单元测试（含契约 schema 校验）
make drill                                 # 端到端业务闭环演练（curl + psql，全程可见）
make drill-failure                         # 故障演练（Redis 停机 / 事件丢失 / 对账重放）
make reconcile                             # 对账；REPLAY=1 重放，REBUILD=1 重建读模型
make outbox-status                         # Outbox 积压与死信
make loadgen CONCURRENCY=50 DURATION=20s   # 下单链路压测
cd web && pnpm test:e2e                    # Playwright e2e（需后端在跑）
```

真实依赖集成测试默认不跑（不连数据库、不写数据），显式开启：

```bash
TOKMALL_RUN_PG_INTEGRATION=1 \
TOKMALL_RUN_REDIS_INTEGRATION=1 \
TOKMALL_TEST_DATABASE_DSN='host=127.0.0.1 user=tokmall password=tokmall dbname=tokmall port=5432 sslmode=disable' \
go test ./internal/order/ ./internal/outbox/ ./internal/reconcile/ -count=1
```

## 三条主故事（面试用）

| 故事 | 一句话 | 证据 |
|---|---|---|
| **一致性** | 条件更新 + 行锁防超卖；订单/支付/退款状态机由条件更新与数据库 CHECK 双层兜底 | `internal/order/postgres_integration_test.go`：100 库存 / 500 并发恰好 100 成功 |
| **可靠事件** | Transactional Outbox + 至少一次投递 + 回执与投影同事务的幂等消费 + DLQ 重放 | `internal/outbox/integration_test.go`：恰好一次投影、重复不累加、崩溃可认领、DLQ 可重放 |
| **可证明性** | 六项对账检查 + 对账驱动重放 + 读模型重建；真实数据上抓到并修复两处不一致 | `internal/reconcile/integration_test.go`、`scripts/drill_failure_modes.sh` |

## 工程化

- **测试分层**：单元（fake，快）→ 真实依赖集成（显式开关、独立 schema）→ e2e（Playwright）。
- **契约同步**：路由 ↔ OpenAPI 双向 diff（`cmd/api/main_test.go`）
  + schema ↔ Go DTO 字段/类型/必填对齐（`internal/api/spec_test.go`，已做变异验证）。
- **CI**：后端（vet/单测/竞态/集成）、契约、前端（生成物漂移/类型/lint/单测/构建）、e2e（`.github/workflows/ci.yml`）。
- **可观测性**：HTTP/连接池/缓存/Outbox 积压与死信/消费延迟/对账差异指标，
  结构化日志（request_id 贯穿），`outboxctl`、`reconcile` 两个运维命令。
- **故障演练**：`scripts/drill_failure_modes.sh` 把"依赖挂了会怎样"变成可复现步骤。

## 文档导航

| 想了解 | 看哪里 |
|---|---|
| **面试讲解主文档**（问题导向，含十个故事与问答速查） | [`docs/interview-walkthrough.md`](docs/interview-walkthrough.md) |
| 项目审核结论与修复计划 | [`docs/project-audit-2026-10.md`](docs/project-audit-2026-10.md)、[`docs/remediation-plan.md`](docs/remediation-plan.md) |
| 全部文档入口与当前进度 | [`docs/README.md`](docs/README.md) |
| 后端架构与模块边界 | [`docs/backend-architecture.md`](docs/backend-architecture.md) |
| 业务范围与技术实现 | [`docs/commerce-business.md`](docs/commerce-business.md)、[`docs/commerce-v1-technical-design.md`](docs/commerce-v1-technical-design.md) |
| 架构取舍（ADR-001～020） | [`docs/architecture-decisions.md`](docs/architecture-decisions.md) |
| 压测与容量结论 | [`docs/performance-baseline.md`](docs/performance-baseline.md) |
| 分模块实施记录与实验 | [`docs/interview-notes/`](docs/interview-notes/) |
| API 契约 | [`internal/api/openapi.yaml`](internal/api/openapi.yaml) |
| 前端说明 | [`web/README.md`](web/README.md) |

## 已知边界

模拟支付（非真实渠道回调）、只支持全额退款、模拟发货无物流、
事件传输默认 Redis Streams（Kafka 为可替换实现）、预签名上传未绑定实际大小、
无告警规则与链路追踪、单机 compose 部署、未做秒杀。
完整清单与"什么时候该做"见 [`docs/interview-walkthrough.md`](docs/interview-walkthrough.md) §15。
