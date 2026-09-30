# TokMall 文档导航

> 更新于 2026-10-01。这里记录**当前实现**；旧方案中的示例 DDL、路由和技术选型不能代替代码。

## 当前进度

- **Commerce v1**（`ab9d062` / `b5349ea`）：商品/SKU/库存、单 SKU 下单、模拟支付、
  取消/超时关单、视频挂商品、Feed/详情商品卡、作者成交统计、买卖双方订单查询与履约。
- **安全与正确性收口**（`ab8c92a`）：修复审计发现的 P0/P1（限流绕过、超时订单可支付、
  并发 refresh 双签发、singleflight 故障放大、输入校验与错误语义等 10 项）。
- **售后闭环**（`6e1dd1e`）：全额退款申请/裁决/库存回补策略，订单状态机新增
  REFUND_REQUESTED/REFUNDED，集成测试覆盖并发裁决与竞争。
- **Transactional Outbox**（`ff288ac`）：事件与业务同事务、至少一次投递、
  幂等消费（回执与投影同事务）、DLQ 与人工重放、作者日销投影、`cmd/outboxctl`。
- **对账与可证明性**（`1436fd5`、`68192ea`）：六项对账检查、对账驱动重放、
  读模型重建、契约 schema 级校验、CI 四个 job、压测客户端与故障演练脚本。
- 仍未实现：真实支付渠道、部分退款/退货入库、佣金结算、秒杀、跨商家带货授权、
  Agent 客服、生产级物流、告警规则与链路追踪。边界清单见
  [`interview-walkthrough.md`](interview-walkthrough.md) §15。

## 阅读顺序与事实来源

| 需要了解 | 看哪里 | 说明 |
|---|---|---|
| **面试讲解主文档（先看这个）** | [`interview-walkthrough.md`](interview-walkthrough.md) | 问题导向的十个故事 + 问答速查 + 边界清单 |
| 审核结论与修复计划 | [`project-audit-2026-10.md`](project-audit-2026-10.md)、[`remediation-plan.md`](remediation-plan.md) | 审计发现（编号 BE-*/FE-*/CT-*）与分阶段计划 |
| 项目结构、里程碑、开发命令 | [`AGENTS.md`](../AGENTS.md) | 项目工作记忆 |
| 当前模块边界和数据所有权 | [`backend-architecture.md`](backend-architecture.md) | 对照 `cmd/api/main.go` 更新 |
| Commerce v1 的当前业务范围 | [`commerce-business.md`](commerce-business.md) | 已实现与后续需求分开 |
| Commerce v1 的当前技术实现 | [`commerce-v1-technical-design.md`](commerce-v1-technical-design.md) | 以现有代码重写 |
| Feed 对照 GCFeed 的后续计划 | [`feed-evolution-plan.md`](feed-evolution-plan.md) | 先正确性与压测，再决定缓存/分发演进；未实现 |
| Mall 对照 DomTok 的后续计划 | [`commerce-evolution-plan.md`](commerce-evolution-plan.md) | 先交易基线，再做售后与工程化；未实现 |
| 准生产级后续工作 | [`interview-production-readiness-plan.md`](interview-production-readiness-plan.md) | 阶段 4 起仍是计划 |
| 架构取舍 | [`architecture-decisions.md`](architecture-decisions.md) | ADR-001～020（014 起为售后/Outbox/对账/安全收口） |
| 压测结论 | [`performance-baseline.md`](performance-baseline.md) | 测试时点的历史数据，不能当作当前性能承诺 |
| API 形状 | [`openapi.yaml`](../internal/api/openapi.yaml) | route-sync 测试检查路由；最终行为以代码和测试为准 |
| 表结构 | [`migrations/`](../migrations/) | 按编号执行，禁止 AutoMigrate |
| 前端运行和测试 | [`web/README.md`](../web/README.md) | React/Vite/Playwright |

## 历史证据

- [`interview-notes/`](interview-notes/)：按实施阶段记录的决策和实验；“本阶段尚未实现”应按该笔记写作时点理解。
- [`performance-baseline.md`](performance-baseline.md)：保留原始条件和结果，旧 QPS 不是当前容量承诺。

旧版 Feed 手册、跨域路线图、项目对比和通用提交速查已从当前文档树移除；早期取舍收敛到 ADR，历史全文仍可从 Git 历史查看。未来工作以两份演进计划与准生产级计划为入口。

## 验证命令

```bash
go test ./...                     # 单元测试 + 契约 schema 校验
make dev                          # 依赖容器 + 后端 + 前端
make drill                        # 端到端业务闭环演练（内容→…→退款→投影）
make drill-failure                # 故障演练（Redis 停机 / 事件丢失 / 对账重放）
make reconcile                    # 对账（REPLAY=1 重放，REBUILD=1 重建读模型）
make loadgen CONCURRENCY=50 DURATION=20s
cd web && pnpm test:e2e
make stop && docker compose stop postgres minio redis
```

真实 PostgreSQL 集成测试需先启动项目 PostgreSQL，并显式设置 `TOKMALL_RUN_PG_INTEGRATION=1` 与 `TOKMALL_TEST_DATABASE_DSN`。测试使用独立 schema，结束时清理。`docs/performance-baseline.md` 中的 5433 容器与旧压测命令仅是历史记录；当前 Compose 使用 5432。
