# TokMall 文档导航

> 更新于 2026-09-26。这里记录**当前实现**；旧方案中的示例 DDL、路由和技术选型不能代替代码。

## 当前进度

- `main` 已完成 Commerce v1：商品/SKU/库存、单 SKU 下单、模拟支付、取消/超时关单、视频挂商品、Feed/详情商品卡、作者成交统计，以及买家/商家订单查询、模拟发货与买家确认完成。阶段 3 的基线提交为 `ab9d062`，订单收尾提交为 `b5349ea`。
- 播放事件异步化曾作为 M5-Kafka 候选，压测后搁置。Transactional Outbox + Kafka 是面试实施计划的下一阶段，尚未开始；它也不是 Commerce v1 正确性的前提。
- 真实支付渠道、退款/售后、佣金结算、秒杀、跨商家带货授权、Agent 客服、生产级物流均未实现。

## 阅读顺序与事实来源

| 需要了解 | 看哪里 | 说明 |
|---|---|---|
| 项目结构、里程碑、开发命令 | [`AGENTS.md`](../AGENTS.md) | 项目工作记忆 |
| 当前模块边界和数据所有权 | [`backend-architecture.md`](backend-architecture.md) | 对照 `cmd/api/main.go` 更新 |
| Commerce v1 的当前业务范围 | [`commerce-business.md`](commerce-business.md) | 已实现与后续需求分开 |
| Commerce v1 的当前技术实现 | [`commerce-v1-technical-design.md`](commerce-v1-technical-design.md) | 以现有代码重写 |
| 准生产级后续工作 | [`interview-production-readiness-plan.md`](interview-production-readiness-plan.md) | 阶段 4 起仍是计划 |
| 压测结论 | [`performance-baseline.md`](performance-baseline.md) | 测试时点的历史数据，不能当作当前性能承诺 |
| API 形状 | [`openapi.yaml`](../internal/api/openapi.yaml) | route-sync 测试检查路由；最终行为以代码和测试为准 |
| 表结构 | [`migrations/`](../migrations/) | 按编号执行，禁止 AutoMigrate |
| 前端运行和测试 | [`web/README.md`](../web/README.md) | React/Vite/Playwright |

## 历史设计与学习记录

- [`feed-system-mentor-guide.md`](feed-system-mentor-guide.md)、[`feed-system-supplement.md`](feed-system-supplement.md)：保留 Feed 的演进思路与 ADR。其早期“推荐目录”“下一步”和 API 草案不是当前实现；当前状态以本页、`AGENTS.md` 和 OpenAPI 为准。
- [`content-commerce-backend-agent-guide.md`](content-commerce-backend-agent-guide.md)：跨 Feed、Commerce、Agent 的原始路线图。Agent、秒杀、Kitex、Kafka 等多为候选，不代表已交付。
- [`comparison-vs-leonincs-projects.md`](comparison-vs-leonincs-projects.md)：特定提交时点的静态对比，不作为今天的功能清单或性能结论。
- [`interview-notes/`](interview-notes/)：按实施阶段记录的决策和实验；“本阶段尚未实现”应按该笔记写作时点理解。

## 验证命令

```bash
go test ./...
cd web && pnpm build && pnpm lint
make dev
cd web && pnpm test:e2e
make stop && docker compose stop postgres minio redis
```

真实 PostgreSQL 集成测试需先启动项目 PostgreSQL，并显式设置 `TOKMALL_RUN_PG_INTEGRATION=1` 与 `TOKMALL_TEST_DATABASE_DSN`。测试使用独立 schema，结束时清理。`docs/performance-baseline.md` 中的 5433 容器与旧压测命令仅是历史记录；当前 Compose 使用 5432。
