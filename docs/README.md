# TokMall 文档导航

> 更新于 2026-09-26。这里记录**当前实现**；旧方案中的示例 DDL、路由和技术选型不能代替代码。

## 当前进度

- `main` 已完成 Commerce v1：商品/SKU/库存、单 SKU 下单、模拟支付、取消/超时关单、视频挂商品、Feed/详情商品卡、作者成交统计，以及买家/商家订单查询、模拟发货与买家确认完成。阶段 3 的基线提交为 `ab9d062`，订单收尾提交为 `b5349ea`。
- 播放事件异步化曾作为 M5-Kafka 候选，压测后搁置。Transactional Outbox + Kafka 是原面试实施计划的阶段 4，尚未开始；是否启动按新演进计划的触发条件判断，它不是 Commerce v1 正确性的前提。
- 真实支付渠道、退款/售后、佣金结算、秒杀、跨商家带货授权、Agent 客服、生产级物流均未实现。

## 阅读顺序与事实来源

| 需要了解 | 看哪里 | 说明 |
|---|---|---|
| 项目结构、里程碑、开发命令 | [`AGENTS.md`](../AGENTS.md) | 项目工作记忆 |
| 当前模块边界和数据所有权 | [`backend-architecture.md`](backend-architecture.md) | 对照 `cmd/api/main.go` 更新 |
| Commerce v1 的当前业务范围 | [`commerce-business.md`](commerce-business.md) | 已实现与后续需求分开 |
| Commerce v1 的当前技术实现 | [`commerce-v1-technical-design.md`](commerce-v1-technical-design.md) | 以现有代码重写 |
| Feed 对照 GCFeed 的后续计划 | [`feed-evolution-plan.md`](feed-evolution-plan.md) | 先正确性与压测，再决定缓存/分发演进；未实现 |
| Mall 对照 DomTok 的后续计划 | [`commerce-evolution-plan.md`](commerce-evolution-plan.md) | 先交易基线，再做售后与工程化；未实现 |
| 准生产级后续工作 | [`interview-production-readiness-plan.md`](interview-production-readiness-plan.md) | 阶段 4 起仍是计划 |
| 早期架构取舍 | [`architecture-decisions.md`](architecture-decisions.md) | ADR-001～013 原文和当前适用性提示 |
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
go test ./...
cd web && pnpm build && pnpm lint
make dev
cd web && pnpm test:e2e
make stop && docker compose stop postgres minio redis
```

真实 PostgreSQL 集成测试需先启动项目 PostgreSQL，并显式设置 `TOKMALL_RUN_PG_INTEGRATION=1` 与 `TOKMALL_TEST_DATABASE_DSN`。测试使用独立 schema，结束时清理。`docs/performance-baseline.md` 中的 5433 容器与旧压测命令仅是历史记录；当前 Compose 使用 5432。
