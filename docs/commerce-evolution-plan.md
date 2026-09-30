# Mall 演进计划：对照 DomTok

> 计划，未实现。核对基点：TokMall `main`（2026-09-26）与 [DomTok `e434d96`](https://github.com/west2-online/DomTok/tree/e434d9657e26f9fe43b6374feb334e95a12fdb3a)。TokMall 已有商品、库存、单 SKU 订单、模拟支付、取消/超时、模拟履约和成交归因；详见 [业务范围](commerce-business.md)、[当前实现](commerce-v1-technical-design.md)。下述功能不得写入“已完成”或简历成果，直到验收通过。

## 1. 选择矩阵

| DomTok 中值得参考的做法 | TokMall 当前情况 | 决定 |
|---|---|---|
| [独立支付单、退款单、资金流水模型](https://github.com/west2-online/DomTok/blob/e434d9657e26f9fe43b6374feb334e95a12fdb3a/app/payment/domain/model/payment.go) 与 [退款用例](https://github.com/west2-online/DomTok/blob/e434d9657e26f9fe43b6374feb334e95a12fdb3a/app/payment/usecase/service.go) | 只有模拟支付事实，无退款/售后，支付后订单不可逆 | 借鉴状态与账本分离，做“模拟退款 + 售后”闭环；金额与权限规则重新设计 |
| [CI 检查](https://github.com/west2-online/DomTok/blob/e434d9657e26f9fe43b6374feb334e95a12fdb3a/.github/workflows/ci.yaml) 与 [统一 tracing 初始化](https://github.com/west2-online/DomTok/blob/e434d9657e26f9fe43b6374feb334e95a12fdb3a/pkg/base/telemetry_provider.go) | 有测试和 Prometheus，但缺稳定 CI 门禁与订单链路追踪 | 借鉴可证明性，先上轻量 CI 与关键交易指标；追踪按排障收益决定 |
| [跨订单服务预扣库存 + 延时消息回补](https://github.com/west2-online/DomTok/blob/e434d9657e26f9fe43b6374feb334e95a12fdb3a/app/order/domain/service/create_order.go) | 订单和库存同一 PostgreSQL 事务，条件扣库存和流水可原子提交 | 不移植。跨 RPC/MQ 的补偿会把当前原子操作变成分布式一致性问题 |
| Kitex 多服务、RocketMQ、Redis 库存预扣、搜索/AI 扩展 | TokMall 是单进程模块化单体，尚无必须拆分的容量或团队边界 | 不按架构数量移植；只在压测证明瓶颈后做独立实验 |

DomTok 的 [支付域服务](https://github.com/west2-online/DomTok/blob/e434d9657e26f9fe43b6374feb334e95a12fdb3a/app/payment/domain/service/service.go) 仍以模拟返回为主，不能把它当作真实第三方支付接入范本。TokMall 当前的买家 `/pay` 也是模拟命令，不能直接改名成公网支付回调。

## 2. 执行顺序

### C0：为交易链路建立容量与对账基线

覆盖商品查询、创建订单、重复幂等请求、支付与取消/到期竞争、商家订单查询、模拟履约、作者统计。固定数据规模、并发、连接池、脚本和机器资源，记录成功率、P95/P99、DB 行锁/池等待、关单批次与积压、归因查询耗时。构造高热点 SKU 与普通散列 SKU 两组，观察是否真有库存行竞争。已有 [500 并发/100 库存正确性实验](interview-notes/order-concurrency-proof.md) 是不超卖证据，不是最大吞吐结论。

增加定期只读对账报告：库存余额与流水、已支付订单与支付事实、取消/过期回补次数、作者 GMV 与订单事实。报告差异及修复流程，先不要自动改写历史订单。

**完成证据**：可重复压测数据和脚本、原始指标、对账 SQL/报告、至少一次支付/取消/过期并发故障复现。此阶段决定 C2/C3 是否需要性能架构改造。

### C1：补模拟售后与退款闭环

先写业务规则和状态机，再做表与接口。建议最小范围是“已支付、未确认完成的单 SKU 整单退款”；是否允许已完成后退款以及退货物流，需单独产品决策。模拟退款不调用真实资金渠道，UI 与 API 明示模拟。

- 建 `refund_requests`、`refund_attempts` 或等价模型，并为每笔模拟资金变动写不可变流水；订单支付事实与退款事实分开。金额从订单快照派生，禁止客户端指定任意退款金额。
- 明确买家申请、商家审核、模拟退款执行和拒绝的权限。订单行锁/条件状态转移处理“申请退款 vs 发货/确认完成”、重复申请、审核重试与重复执行；使用业务幂等键或唯一约束使资金流水只记一次。
- 定义库存与归因口径：付款后退款是否回补库存、何时回补、GMV 是净额还是总额；在 `orders`、库存流水、作者统计之间保持同一规则。先做整单，部分退款留后续。
- 前端提供申请与审核入口，订单详情显示退款进度和模拟标记。更新 OpenAPI、迁移、权限与真实 PG 并发测试、Playwright 主链路。

**完成证据**：重复请求只退一次；退款与履约竞争只能产生合法状态；金额/库存/作者统计可对账；非买卖双方无权操作；失败可安全重试。不要直接复制 DomTok 的状态枚举或金额计算。

### C2：CI、可观测性与故障演练

在现有 Go/前端测试基础上，让 CI 跑 `go test ./...`、必要的 race 与隔离 PG 集成测试、迁移 up/down 验证、OpenAPI route-sync、前端 build/lint/e2e。为订单成功/失败、库存不足、幂等重放、状态冲突、超时关单积压、退款重试/失败、对账差异加指标。日志用 request/order/idempotency ID 串起链路；不记 JWT 与支付敏感数据。先用日志与指标定位问题，只有跨进程或排障数据证明需要时才加 tracing。

演练 PG 不可用、Redis 不可用、Worker 重启和支付/退款模拟执行中断，记录用户看到的结果、重试方法和数据是否一致。与 [现有阶段 5](interview-production-readiness-plan.md) 合并实施，避免两套 CI/指标计划并存。

**完成证据**：主分支必过门禁、故障记录和恢复步骤、关键交易指标有可读仪表板。

### C3：Outbox 与热点库存，只在触发条件满足时启动

- [现有阶段 4](interview-production-readiness-plan.md) 的 Outbox 可用于支付成功后的非核心副作用（通知、异步归因投影），不是支付/库存事务的替代。若启动，支付事务同写事件，relay 重试，consumer 用 `event_id` 幂等，DLQ 可重放；支付成功不依赖 Kafka 可用。
- 热点 SKU 若在 C0 实测显示数据库行锁是主要瓶颈，再建**隔离实验**对比当前条件 `UPDATE`、入口限流、Redis 预扣 + 排队方案。核对一人一单、超卖、下单失败回补、消息丢失/重复、库存账实一致和用户等待体验。只有收益与补偿成本都可量化才考虑进入主链路。
- 多进程部署先验证状态共享、关单 Worker 并发、缓存和限流行为；这不要求先拆 Kitex 服务。模块边界与数据所有权稳定后，再讨论服务拆分。

**进入条件**：C0/C2 数据明确指向库存热点、异步副作用延迟或单进程容量不足，并写清目标 SLO 与恢复预算。否则保持当前同库事务。

## 3. 与已有计划的关系

本文件只补充 DomTok 对照后的 Mall 取舍与优先级；Commerce v1 的事实在 [当前技术实现](commerce-v1-technical-design.md)，Outbox 和工程化的详细验收继续在 [准生产级计划](interview-production-readiness-plan.md)。建议先 C0，再按业务需求做 C1 与 C2；C3 由测量结果触发。每阶段记录实现提交、真实 PG 并发结果、压测环境和当前未解决问题，简历只引用已验收的结果。
