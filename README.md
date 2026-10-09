# TokMall

CloudWeGo Hertz 的短视频 Feed + 商城学习项目，采用模块化单体。PostgreSQL 是业务事实源，Redis 承担缓存、限流与事件传输，RustFS 保存媒体对象；React 前端用于演示核心链路。

```text
内容：申请上传 → 临时对象直传 → 服务端封存 → 异步媒体校验 → 作者发布 → Feed / 互动
交易：本人视频挂本人商品 → 单 SKU 下单 → 模拟支付 → 发货 → 确认完成
售后：待发货 / 已发货订单 → 申请全额退款 → 商家裁决 → 按规则回补库存、退券
恢复：业务事实 + Outbox 同事务 → Redis Streams → 幂等投影 → 九项对账 / 重放 / 重建
```

确认完成后不接受退款申请。模拟支付、模拟履约和退款裁决不涉及真实资金或物流。

## 启动与验证

Go 版本以 [go.mod](go.mod) 为准；前端使用 Node 22（与 CI 一致）、pnpm 11。本地需 Docker 和相应依赖；真实媒体校验需 ffprobe/ffmpeg。

```bash
cd web && pnpm install --frozen-lockfile
cd ..
make dev
# 前端 :5173；API :8080；指标 127.0.0.1:9091/metrics
make stop
```

`make dev` 使用开发密钥与默认本地凭证；生产配置另有强校验。默认开启媒体校验，上传后不一定立即可发布。首次环境准备、Docker 凭证问题、测试开关与旧部署升级见 [开发与运维](docs/development-and-operations.md)。

```bash
go test ./...                     # 默认跳过真实依赖集成测试
make lint                         # 需安装仓库配置匹配的 golangci-lint
make verify                       # fmt / vet / lint / test / tidy-check
cd web && pnpm typecheck && pnpm exec vitest run
```

## 项目可以证明什么

| 问题 | 当前机制与证据 |
|---|---|
| 并发交易正确性 | 条件扣库存、订单行锁、状态机与 DB CHECK；[真实 PG 测试](internal/order/postgres_integration_test.go) |
| 事件失败恢复 | Outbox 至少一次投递，消费回执与投影同事务；[集成测试](internal/outbox/integration_test.go) |
| 账实一致 | 九项检查、缺失回执重放、作者日销重建；[对账测试](internal/reconcile/integration_test.go) |
| Feed 分页与缓存 | 时间游标、关注完整时间线、热榜冻结分数游标、L1/L2；[实现与测试](internal/feed/) |
| 媒体不可替换与删除补偿 | 上传签名绑定长度/类型、ETag 封存、发布门禁、持久化清理；[RustFS 测试](internal/storage/upload_integration_test.go)、[PG 测试](internal/video/cleanup_postgres_integration_test.go) |

历史性能数字均带测量条件，见 [性能基线](docs/performance-baseline.md)，不等于当前容量承诺。源码包含 Prometheus 规则和 traceparent 关联，不代表部署了完整 OTel tracing 或告警通知链路。

## 阅读入口

从 [文档导航](docs/README.md) 按目的阅读：架构、业务与事务设计、ADR、开发运维、性能证据、面试讲解、后续计划各只有一个入口。[AGENTS.md](AGENTS.md) 是修改代码的工作约定，不重复承担整套项目说明。

后端已有优惠券、热榜与媒体校验；前端尚无领券/选券界面、热榜入口和完整校验状态展示。真实渠道支付、部分退款、退货入库、佣金结算、累计上传配额等边界见 [后续计划](docs/roadmap.md)。
