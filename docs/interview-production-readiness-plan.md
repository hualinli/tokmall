# TokMall 准生产级（面试）实施计划

> 状态：执行中  
> 目标：在不过度设计、不虚构业务规模的前提下，把 TokMall 建设为可用代码、测试、压测与故障演练自证的内容电商后端项目。

## 1. 最终交付主线

```text
内容生产与 Feed
        ↓
视频挂商品
        ↓
普通订单 + 条件扣库存防超卖
        ↓
支付 / 取消 / 超时竞争
        ↓
库存回补与流水对账
        ↓
Transactional Outbox + Kafka
        ↓
带货归因、统计等异步消费方
```

“准生产级”在本项目中的含义：核心业务正确；权限、并发、幂等和失败语义明确；关键结论有自动化测试或实验数据；具备观测与恢复手段；所有简化均通过 ADR 诚实记录。它不代表系统已经达到真实大型电商的流量和组织复杂度。

## 2. 执行与提交纪律

每个模块严格执行以下流程：

1. 开始前明确范围、非目标和验收条件。
2. 由一个实现子任务完成代码、测试和必要文档，不并行修改相邻模块。
3. 主审查者独立检查安全、正确性、并发、错误语义、迁移兼容性和契约一致性。
4. 运行与风险相称的测试；涉及并发时必须增加并发或真实数据库验证。
5. 审查问题修正完毕后，以单一主题提交，禁止把多个阶段混进一次提交。
6. 每个模块增加一份面试讲解，至少回答：
   - 解决了什么业务问题；
   - 为什么采用当前方案；
   - 核心不变量是什么；
   - 并发、重试和部分失败时会怎样；
   - 为什么没有采用更复杂的替代方案；
   - 当前边界和未来演进是什么。

提交信息采用 Conventional Commits，推荐形式：

```text
fix(video): enforce author ownership on publish
feat(order): create order with atomic stock deduction
test(order): verify no overselling under concurrency
docs(order): explain transaction and idempotency design
```

原则上“实现 + 对应测试”可以同一提交；面试讲解如果内容较大则单独使用 `docs(...)` 提交。任何阶段开始前工作区必须可解释，提交后必须保持可构建、可测试。

## 3. 阶段 0：现有系统安全与一致性收口

目标：先让现有内容、Feed 和库存骨架可信，再承接订单链路。

### 0.1 视频发布授权

- 发布条件必须包含 `video_id + author_id + from_status`。
- 明确不存在、非作者和状态冲突的对外语义。
- 覆盖越权发布和并发发布测试。

### 0.2 互动与用户可见性

- 仅允许点赞存在且为 `PUBLISHED` 的视频。
- 软删除或禁用用户不得继续进入关注关系和统计 read model。
- 形成统一的“可交互用户/可互动视频”定义。

### 0.3 Feed 正确性

- 修复 singleflight 共享结果被请求级字段修改的数据竞争。
- 关注/取关后失效关注 Feed 首页缓存。
- 在热度排序具备正确游标前，禁止将其作为支持深分页的正式能力。

### 0.4 播放事件幂等

- `event_id` 必须是合法 UUID。
- 相同幂等键、相同请求返回成功；相同键、不同请求返回冲突。

### 0.5 商品与库存地基

- 修复库存幂等键的作用域和请求一致性校验。
- SKU 创建时同步初始化库存，消除首次调整竞态。
- 恢复库存非负与必要状态 CHECK 约束。
- 区分开发默认配置和生产强制配置。

### 阶段验收

- 已知 P0 清零，关键 P1 均已修复或有明确 ADR。
- 后端全量测试、race 检查、前端测试和路由契约同步通过。
- 每项修复均有回归测试和面试讲解。

## 4. 阶段 1：普通订单与防超卖

第一版只支持单 SKU，不加入优惠、退款、购物车和拆单。

### 4.1 数据模型

- `orders`：订单号、用户、状态、总金额、幂等键、请求指纹、来源视频/作者及时间字段。
- `order_items`：商品、SKU、标题、规格、单价、数量和商家快照。
- `payments`：支付尝试、第三方交易号、金额和状态。
- `video_products`：视频与商品的受控关联。

### 4.2 创建订单事务

在同一 PostgreSQL 事务内完成：

1. 校验商品、SKU 和请求数量；
2. 使用服务端价格生成订单快照；
3. 条件扣库存：`UPDATE ... WHERE quantity >= requested`；
4. 写库存流水；
5. 创建订单及订单项；
6. 一起提交或一起回滚。

### 4.3 下单幂等

- 幂等范围为当前用户。
- 保存请求指纹；同键同请求返回原订单，同键不同请求返回冲突。
- 并发重试只能创建一个订单、扣减一次库存。

### 阶段验收

真实 PostgreSQL 并发实验：库存 100、500 个并发单件订单，必须恰好 100 个成功，最终库存为 0，订单项合计为 100，库存流水合计为 -100，且不存在负库存或半订单。

## 5. 阶段 2：支付、取消与超时关单

订单状态机：

```text
PENDING_PAYMENT → PAID
PENDING_PAYMENT → CANCELED
PENDING_PAYMENT → EXPIRED
```

- 所有迁移使用带来源状态的条件更新。
- 模拟支付回调必须处理重复、金额不一致、晚到和不存在订单。
- 取消/过期与库存回补在同一事务中完成，并通过流水业务键保证只回补一次。
- 超时 Worker 使用批量扫描和 `FOR UPDATE SKIP LOCKED`，支持多实例与优雅停机。

### 阶段验收

- 重复支付只生效一次。
- 支付与取消、支付与过期并发时只允许一个合法终态。
- 多个关单 Worker 不重复回补。
- 库存余额、订单明细与库存流水可对账。

## 6. 阶段 3：内容电商闭环

- 作者只能给自己的已发布视频挂自己的在售商品。
- Feed 按页批量装配商品卡，禁止 N+1。
- 订单记录来源视频和来源作者。
- 支付成功形成成交归因。
- 提供作者支付订单数、销量和 GMV 的基础统计。

验收演示链路：上传视频 → 创建商品/SKU → 补库存 → 视频挂商品 → Feed 浏览 → 下单 → 支付 → 库存变化 → 作者统计变化。

## 7. 阶段 4：Transactional Outbox 与 Kafka

引入 MQ 的理由是可靠分发支付后的非核心副作用，而不是虚构数据库吞吐瓶颈。

### 4.1 范围

- 一个 Topic：`commerce.order-events.v1`。
- 一个核心事件：`OrderPaid`。
- 支付事务同时写入 `outbox_events`。
- Relay 投递并记录发布状态。
- 统计 Consumer 使用 `event_id` 唯一约束实现幂等消费。
- 定义有限重试、DLQ 和人工重放流程。

### 4.2 故障实验

- Kafka 停机时支付仍正确完成，事件保留在 Outbox。
- Kafka 恢复后事件补发。
- Consumer 处理后、提交 offset 前崩溃，重复投递不重复增加 GMV。
- 永久失败事件进入 DLQ，并可安全重放。

## 8. 阶段 5：工程化与可证明性

- 单元测试、真实依赖集成测试、并发测试和完整 E2E 分层。
- CI 执行 Go 测试、race、前端测试、OpenAPI route-sync、迁移验证和 lint。
- 增加订单成功率、库存不足、幂等重放、状态冲突、关单、Outbox backlog、消费延迟和 DLQ 指标。
- 日志关联 request/user/order/idempotency/event 标识，禁止记录密码、JWT 和敏感支付载荷。
- 演练 Redis、MinIO、PostgreSQL、Kafka 和 Consumer 故障，记录用户表现、指标与恢复步骤。

## 9. 阶段 6：简历与面试交付

- 根 README：项目定位、架构图、主链、启动方式、压测结果、故障实验和已知边界。
- 准备三条主讲故事：Feed 性能演进、订单库存一致性、Outbox 可靠事件。
- 为每个模块保留可独立阅读的面试讲解，结论必须能回指代码、测试或实验数据。

## 10. 可选阶段：秒杀演进实验

秒杀不是约面前置条件。只有数据库热点库存行经压测出现可测量瓶颈后，再评估入口限流、Redis Lua 预扣、一人一单、Kafka 排队、异步下单、失败补偿和对账。

如果业务数据不足以触发瓶颈，可以在独立实验中对比数据库直扣与 Redis 预扣，但必须将其描述为架构演进验证，不能宣称为真实生产需求。

## 11. 预计节奏

| 周次 | 目标 | 关键证据 |
|---|---|---|
| 第 1 周 | 阶段 0 安全与一致性收口 | P0 清零、回归测试 |
| 第 2 周 | 创建订单与防超卖 | 500 并发/100 库存实验 |
| 第 3 周 | 支付、取消和超时关单 | 状态竞争与只回补一次 |
| 第 4 周 | 视频挂商品与归因 | 内容电商完整演示 |
| 第 5 周 | Outbox + Kafka | 停机补发和重复消费实验 |
| 第 6 周 | CI、观测和故障演练 | 自动化与恢复记录 |
| 第 7 周 | README、简历和面试材料 | 可投递版本 |

阶段 0 与阶段 1 完成后可以小规模投递；阶段 3 完成后进入正常约面；MQ 与工程化可以在投递期间继续演进。

## 12. 进度记录

| 模块 | 状态 | 实现提交 | 讲解文档 | 验收结果 |
|---|---|---|---|---|
| 实施计划 | 完成 | `2441f09` | 本文 | 静态审阅形成路线 |
| 0.1 视频发布授权 | 完成 | `96139af` | `docs/interview-notes/video-publish-authorization.md` | video/cmd-api 测试 + video race 通过 |
| 0.2A 点赞视频可见性 | 完成 | `6b76a74` | `docs/interview-notes/interaction-video-visibility.md` | interaction/cmd-api 测试 + interaction race 通过 |
| 0.2B 软删除用户互动过滤 | 完成 | `f0625c2` | `docs/interview-notes/deleted-user-interaction-visibility.md` | interaction/user/cmd-api 测试 + interaction/user race 通过 |
| 0.3A Feed 缓存请求隔离 | 完成 | `721ba84` | `docs/interview-notes/feed-singleflight-isolation.md` | feed/cmd-api 测试 + feed race 通过 |
| 0.3B 关注 Feed 缓存失效 | 完成 | `2f97345` | `docs/interview-notes/following-feed-cache-invalidation.md` | interaction/feed/cmd-api 测试与 race 通过 |
| 0.3C HotRanker 分页语义 | 完成 | `6baabd3` | `docs/interview-notes/hot-ranker-pagination-boundary.md` | config/feed/cmd-api race 通过；生产仅允许 time |
| 0.4 播放事件幂等 | 完成 | `91049d8` | `docs/interview-notes/play-event-idempotency.md` | video/cmd-api 测试 + video race 通过 |
| 0.5A 库存幂等与数据库不变量 | 完成 | `d1e1cd9` | `docs/interview-notes/inventory-idempotency-and-invariants.md` | inventory/product/cmd-api 测试 + inventory/product race 通过 |
| 0.5B 生产配置收口 | 完成 | `95a5051` | `docs/interview-notes/production-config-validation.md` | config/cache/cmd-api 测试 + config/cache race 通过；生产依赖 TLS |
| 1A 订单数据模型 | 完成 | `db061ec` | `docs/interview-notes/order-data-model.md` | order/cmd-api 测试 + order race 通过；真实 PG 迁移待 1C 并发实验验证 |
| 1B 创建订单事务与幂等 | 完成 | `f5e4438` | `docs/interview-notes/order-create-transaction.md` | order/inventory/product/cmd-api 测试 + order race 通过；真实并发留待 1C |
| 1C 订单 API 与真实并发实验 | 完成 | `3aa6f0f` | `docs/interview-notes/order-concurrency-proof.md` | 全量测试 + order/cmd-api race；真实 PG：500 并发/100 库存恰好 100 成功，幂等并发仅扣一次 |
| 2A 支付与用户取消竞争 | 完成 | `1fef9a2` | `docs/interview-notes/payment-cancel-race.md` | order/cmd-api 测试与 race；真实 PG 支付/取消竞争、重复回调和单次回补通过 |
| 2B 支付/取消 API | 完成 | `ec0b2d5` | `docs/interview-notes/payment-cancel-api-security.md` | 全量测试 + order/response/cmd-api race；真实 PG 越权支付/取消无副作用 |
| 2C 超时关单 Worker | 待开始 |  |  |  |
| 3 内容电商闭环 | 待开始 |  |  |  |
| 4 Outbox 与 Kafka | 待开始 |  |  |  |
| 5 工程化与可证明性 | 待开始 |  |  |  |
| 6 简历与面试交付 | 待开始 |  |  |  |
