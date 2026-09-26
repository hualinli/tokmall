# Commerce v1 当前技术实现

> 更新于 2026-09-26。本文只描述已实现的模块化单体。原始方案中的示例 DDL、`author_sales_stats` 物化表、真实支付回调和 Kafka 管线已从本文移除；历史讨论可从 Git 历史查看。路由与数据结构以 [`OpenAPI`](../internal/api/openapi.yaml) 和 [`migrations`](../migrations/) 为准。

## 1. 范围和交付状态

Commerce v1 已完成：商品/SKU/库存、单 SKU 订单、买家模拟支付、取消/超时关闭、视频挂商品、Feed/视频详情商品卡、作者成交统计，以及买家/商家订单查询、模拟发货、买家确认完成和前端订单页。阶段 3 基线为 `ab9d062`，订单收尾为 `b5349ea`。

Commerce v1 不包含真实支付渠道、真实物流、退款/售后、佣金结算、秒杀或跨商家推广授权。Outbox + Kafka 是独立的后续工程化阶段；[播放事件 Kafka 化](performance-baseline.md)因缺少当前性能动机而搁置。

## 2. 模块边界

| 模块 | 拥有的写入与规则 | 跨模块读取方式 |
|---|---|---|
| `product` | 商品、SKU、上下架 | `order` 使用自己的在售 SKU 只读投影 |
| `inventory` | 商家库存调整与流水幂等 | `order` 在同一 PostgreSQL 事务中条件扣减/回补库存和写流水 |
| `order` | 订单、订单项快照、支付事实、状态机、超时关单 | 来源视频与作者只保存 ID 快照 |
| `promotion` | `video_products` 的挂载、排序、取消 | `feed`/`video` 使用各自的商品卡 read model |
| `attribution` | 无交易写入；查询作者成交统计 | 直接只读 `orders`、`order_items`，不依赖其他模块的 model 类型 |

所有依赖在 `cmd/api/main.go` 装配。商家是商品所有者（`merchant_id`），作者是视频及归因来源（`author_id`）；两个概念即使在当前“只挂本人商品”的授权简化下也不混用。

## 3. 数据与迁移

| 迁移 | 当前用途 |
|---|---|
| `000007`～`000011` | 商品/SKU、库存/流水及其幂等与数据库不变量；`000008`～`000010` 还调整了约束、封面键和播放事件语义 |
| `000012` | `video_products`、`orders`、`order_items`、`payments`；单 SKU 订单项唯一约束、订单状态及时间戳 CHECK、幂等唯一键 |
| `000013` | 数据库触发器保证同一视频最多 5 个有效商品挂载，并发请求不能绕过上限 |
| `000014` | `fulfilling_at`、`completed_at` 及状态/时间戳约束；买家和商家订单列表索引 |

订单项在创建时保存商品标题、规格、单价、商家 ID 和数量快照。历史订单展示与金额核验不回查当前商品。来源 `video_id`、`author_id`、`product_id` 也是下单事实快照；视频删除或商品下架不改写历史归因。

## 4. 交易链路和状态机

```text
PENDING_PAYMENT ──模拟支付──▶ PAID ──商家模拟发货──▶ FULFILLING
       │                                                   │
       ├──买家取消──▶ CANCELED                             └──买家确认──▶ COMPLETED
       └──超时关单──▶ EXPIRED
```

- `POST /api/v1/orders` 的用户身份来自 Bearer Token。相同 `(user_id, idempotency_key)` 和相同请求指纹返回原订单；复用 key 但改载荷返回冲突。订单、订单项、条件扣库存和库存流水在一个 PostgreSQL 事务中完成；并发买家不能超卖。
- `POST /api/v1/orders/{id}/pay` 是**受 JWT 保护的买家模拟支付命令**，不是公网支付渠道回调。订单锁、支付号/模拟渠道交易号幂等、金额核验和 `PENDING_PAYMENT → PAID` 同属一笔事务。真实渠道接入需要独立适配器先验签、校验时间窗及商户金额，再提交可信支付事实；详见 [支付安全边界](interview-notes/payment-cancel-api-security.md)。
- 买家取消与超时 Worker 都只处理待支付订单。订单状态、库存回补和流水在一个事务中提交；Worker 用 `FOR UPDATE SKIP LOCKED` 多实例扫描。支付、取消和关单由订单行锁与条件状态更新仲裁。
- 商家可将 `PAID → FULFILLING`，买家可将 `FULFILLING → COMPLETED`。转移由归属条件 `FOR UPDATE` 锁定，重复同一操作返回 `replayed=true`；非本人订单与不存在订单同为 404。`000014` 约束履约时间戳与状态一致。完成后的订单不能取消或回补库存。

## 5. 查询、推广和统计

- 系统提供 `GET /api/v1/orders`、`GET /api/v1/orders/{id}` 与商家侧 `/api/v1/merchant/orders`、`/api/v1/merchant/orders/{id}`。列表按 ID 倒序，`before_id` 是上一页最后一条，最多 50 条；批量查询订单项，避免 N+1。买家按 `user_id`、商家按 `merchant_id` 在 SQL 中限定可见范围。
- 作者只能给自己的已发布视频挂自己创建的在售商品。挂载幂等，取消后允许在商品下架或视频不可售时清理关系。Feed 按页批量装配商品卡；缓存的 Feed 主体与动态商品卡分开，下架商品卡保留但不可购买。
- `GET /api/v1/authors/{id}/sales` 仅作者本人可见，只计 `PAID`、`FULFILLING`、`COMPLETED` 且有支付事实的订单。返回订单数、去重买家数、销量、GMV，以及按视频和商品拆分。统计从订单事实实时计算，在只读 `REPEATABLE READ` 快照中保持各分组一致；当前没有 `author_sales_stats` 物化表。
- 前端可从商品卡进入详情并下单模拟支付，也提供订单列表、详情、商家模拟发货和买家确认完成。操作均写明“模拟”，不暗示真实收款或物流。

## 6. 验证与后续边界

`go test ./...` 和 OpenAPI route-sync 覆盖接口契约。订单、推广和归因有显式开启的隔离 PostgreSQL 集成测试；其中订单测试覆盖 500 并发/100 库存、幂等并发、支付/取消/过期竞争、买卖双方权限、分页及并发发货。前端 Playwright 覆盖视频挂商品到支付、发货、完成和作者统计的浏览器链路。运行方式见 [文档导航](README.md)。

后续若测得写吞吐、跨服务事务或查询聚合成为瓶颈，再评估 Outbox + Kafka、异步统计投影与对账任务。真实渠道回调必须独立设计鉴权与审计；不能把当前买家模拟支付端点改名当作回调。
