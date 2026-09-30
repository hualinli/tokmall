# 售后（退款）闭环：状态机、库存策略与一次真实缺陷

## 这一步交付了什么

售后补上了"付了钱、商家不发货"这条断链。命令入口注册在 `internal/order/handler.go`：

```text
POST /api/v1/orders/:id/refund                    买家申请
GET  /api/v1/orders/:id/refund                    买家查询
GET  /api/v1/merchant/orders/:id/refund           商家查询
POST /api/v1/merchant/orders/:id/refund/approve   商家同意
POST /api/v1/merchant/orders/:id/refund/reject    商家拒绝
```

业务范围按 ADR-015 刻意收窄：只做全额退款（`Amount: order.TotalAmount`，见
`internal/order/refund_repository.go`），商家同意即视为渠道退款成功（模拟渠道，与模拟支付对称），
`COMPLETED` 之后不接受申请，不做退货物流。`RequestRefundRequest` 只收 `reason`，
`user_id`/金额都不接受客户端提交，身份只从认证上下文取（`internal/order/refund_handler.go`）。

## 为什么订单主状态要长出 REFUND_REQUESTED / REFUNDED

退款是"申请 → 裁决 → 执行"三段式，天然有两个状态维度：订单履约维度、售后单据维度。
如果只在 `refunds` 表里记状态、订单状态不动，那么"退款中"的订单仍然可以被发货——
除非每条履约命令都先查一次退款表，而那正是"先查后写"的竞态窗口。

所以把售后状态投影进订单主状态（`migrations/000015_order_refund.up.sql`）：

- 订单状态加 `REFUND_REQUESTED` / `REFUNDED`；
- `refund_from_status` 记住进入退款前的状态（只允许 PAID / FULFILLING），拒绝时退回它；
- 于是"退款中不可发货"由订单状态机**本身**保证——状态机就是那把互斥锁，
  履约命令不需要额外查询，也就没有额外的竞态窗口。

数据库同一迁移里收紧三条 CHECK：`orders_status_check` 扩枚举；`orders_refund_columns_check`
要求"退款态 ⟺ `refund_from_status` 非空"且取值只能是 PAID/FULFILLING；
`orders_fulfillment_timestamps_check` 从"状态与时间戳同真同假"改成"时间戳存在则状态必须
已经历过该阶段"——因为发货后退款不能把 `fulfilling_at` 抹掉，历史事实必须保留。

## 核心不变量

1. **同一订单最多一笔"未决或已成功"的退款**：

   ```sql
   CREATE UNIQUE INDEX idx_refunds_order_active
       ON refunds (order_id) WHERE status IN ('REQUESTED', 'SUCCEEDED');
   ```

   被拒绝的退款单不在索引里，所以"拒绝后可以重新申请"，而并发重复申请由唯一索引仲裁，
   而不是靠应用层先查后写（`RequestRefund` 里用 `ON CONFLICT ... TargetWhere` 命中该部分索引）。
2. **所有退款命令先锁订单行**：`SELECT ... FOR UPDATE`（`clause.Locking{Strength: "UPDATE"}`），
   把"检查状态"和"改状态"放进同一把行锁；`rowsAffected != 1` 一律返回 `ErrOrderStateConflict`。
   部分唯一索引是第二道防线：将来新增一条忘记加锁的路径也不会产生两笔并行退款。
3. **拒绝即回滚到申请前状态**：`rejectRefund` 用条件更新把 `status` 写回 `refund_from_status`
   并清空该列，因此 PAID 可以继续发货、FULFILLING 可以继续收货。
4. **库存策略由业务语义决定**：未发货退款回补库存（货还在仓库），已发货退款不回补
   （货已出库，退货入库需要独立的质检/上架流程）。判断依据是 `refund_from_status == PAID`，
   不是调用方传入的参数。
5. **回补最多一次**：回补写 `ORDER_REFUNDED` 流水，业务键是 `(sku_id, reason, order_no)`，
   沿用 000011 的 `(sku_id, reason, biz_id)` 唯一约束；若唯一约束命中，
   `restoreStock` 直接返回错误让整个事务回滚（`internal/order/refund_repository.go`），
   而不是默默再补一次库存。
6. **退款成功同样走 Outbox**：`outbox.EnqueueOrderRefunded` 与状态变更、库存回补在同一事务里
   提交或回滚，载荷里带 `restore_stock`，消费者和对账不需要自己推断。

## 一次真实缺陷：状态可达性 ≠ 命令权限

完整状态图 `orderStatusTransitions`（`internal/order/service.go`）必须包含
`REFUND_REQUESTED → FULFILLING` 这条边——商家**拒绝**退款时订单要退回原状态。
但履约命令最初复用了同一张表，于是商家可以在退款中直接发货，绕过售后裁决。

这不是推演出来的场景：真实 PG 集成测试跑到了它，应用层放行、数据库把它拦了下来——
`orders_refund_columns_check` 要求"`refund_from_status` 非空 ⟺ 状态是退款态"，
订单被写成 FULFILLING 时该列为非空，约束直接报错，漏洞因此暴露。

修法是把两个概念拆开：

```go
var orderStatusTransitions = ...   // 完整状态图：描述状态之间能不能到达（含售后边）
var fulfillmentTransitions = ...   // 商家发货 / 买家确认收货两条命令允许的迁移
```

履约仓储改用 `canFulfillTransition`（`internal/order/read_lifecycle.go`），退款中/已退款订单
在这里没有出口；单测 `TestFulfillmentTransitionsAreNarrowerThanStateGraph`
（`internal/order/refund_service_test.go`）除了断言具体边，还遍历 `fulfillmentTransitions`，
要求每条履约边都必须在完整状态图里存在，防止两张表各自漂移。

## 真实依赖实验（`internal/order/refund_postgres_integration_test.go`）

```bash
TOKMALL_RUN_PG_INTEGRATION=1 \
TOKMALL_TEST_DATABASE_DSN='host=127.0.0.1 user=tokmall password=tokmall dbname=tokmall port=5432 sslmode=disable' \
go test ./internal/order -run TestPostgresRefundProof -count=1 -v
```

`TestPostgresRefundProof` 在随机命名的独立 schema 里跑全部迁移，覆盖九个场景：

- **approve refund restores stock exactly once**：50 个并发"同意"只有 1 次真正执行（其余是幂等重放），
  库存回到期初、`ORDER_REFUNDED` 流水恰好 1 条；
- **shipped refund keeps stock deducted**：发货后退款成功，库存保持已扣减、回补流水 0 条；
- **reject then re-request restores stock once**：拒绝后退回 PAID，二次申请再退款，库存只补一次、
  退款单 2 行（被拒的 + 成功的）而成功退款恒为 1；
- **refund request and fulfillment serialize on order state**：退款申请 vs 发货竞争，
  无论谁赢订单都停在 `REFUND_REQUESTED`；退款先赢时发货拿到 `ErrOrderStateConflict`，
  且事后再发货/确认收货都被状态机挡住；
- **refund request and completion choose one winner**：申请 vs 确认收货只有一个成功，
  另一个分别是 `ErrOrderStateConflict` / `ErrRefundNotAllowed`；
- **concurrent refund requests create one document**：50 个并发申请返回同一个 `RefundID`，
  库里只有 1 行退款单；
- **payment and refund emit outbox events in the same transaction**：支付与退款各留下一条事件，
  金额不符的支付回滚后该订单 0 条事件（事件不会凭空出现）；
- **only buyer requests and only merchant decides**：他人申请/他人裁决/他人查询一律 `ErrOrderNotFound`，
  越权操作对状态与库存零副作用；
- **refunded order cannot be paid or fulfilled again**：终态之后不能再支付、不能再发货、
  也不能二次裁决（重复"拒绝"返回 `ErrRefundAlreadyDecided`）。

断言不是只看一个字段：`assertRefundFacts` 同时校验订单终态、库存余额、退款单行数、
成功退款数、`ORDER_CREATED` 流水恰好一条，并要求"期初 + 流水变更合计 = 当前库存"的守恒。
只校验其中一个，都可能放过"状态对了但货不对"的实现。

## 已知边界

- 不支持部分退款、退货入库、7 天无理由与真实资金渠道（ADR-015）；`COMPLETED` 后不可申请。
- 退款金额从订单快照派生、不接受客户端指定；没有"超时自动同意"这类自动裁决。
- 退款渠道是模拟的："商家同意"即视为渠道退款成功，没有对账渠道回执这一步。
- 对账侧只校验"订单售后状态 ↔ 退款单 ↔ 支付金额"三者一致（`internal/reconcile`），
  不校验渠道侧资金流水（本项目没有真实渠道）。
