# 对账与读模型重建：让"数据是对的"变成可执行的问题

## 这一步交付了什么

上面的状态机、幂等、Outbox 都可能在极端情况下出错：手工改库、补偿脚本漏写流水、
broker 丢数据、消费者 bug。对账回答的是面试里最难兜住的那句追问——
**你怎么知道现在数据是对的？**

- `internal/reconcile`：六项可执行检查 + 对账驱动的重放 + 读模型重建；
- `cmd/reconcile`：一次性执行，`--json` / `--replay` / `--rebuild` / `--grace`；
- `cmd/api/main.go` 里按 `RECONCILE_INTERVAL`（默认 5 分钟）周期运行并开启重放，
  让"数据是否一致"成为持续可观测的事实，而不是上线前跑一次的手工动作。

## 六项检查：每条都是能返回差异行的 SQL

`Repository.RunCheck` 执行的是原始 SQL，差异行被原样保留成
`{"columns": {...}}`，报告既能告警也能直接指导修复。单条检查最多返回 50 行差异
（`maxDifferencesPerCheck`），全量留给人工 SQL。`checks()` 的顺序刻意按
"资金 → 库存 → 履约 → 投影 → 事件"排列：越靠前的差异越严重。

| 检查 | 断言的不变量 | SQL 思路 |
|---|---|---|
| `inventory_balance` | 库存余额 = 流水累计变更 | `inventory LEFT JOIN inventory_transactions` 按 SKU 聚合，`HAVING i.quantity <> COALESCE(sum(t.change), 0)` |
| `order_stock_ledger` | 非待支付订单恰好一条下单流水，回补条数与终态匹配 | `LEFT JOIN` 后按 `reason` 做 `count(*) FILTER (WHERE ...)`，分别断言 ORDER_CREATED/CANCELED/EXPIRED/REFUNDED 的条数；已发货退款必须 0 条回补 |
| `refund_order_consistency` | 订单售后状态 ↔ 退款单 ↔ 支付金额三方一致 | 关联 `refunds`（只取 REQUESTED/SUCCEEDED）与 `LATERAL` 聚合的成功支付；断言状态配对、退款金额 = 订单金额 = 成功支付金额 |
| `order_payment_consistency` | 已支付订单恰好一笔成功支付，未支付订单不得有 | 按订单聚合 `payments`，`HAVING` 按状态分组断言 1 或 0 |
| `author_sales_projection` | 事件驱动投影与订单事实一致（毛额与退款分列） | 三个 CTE（live / live_refunds / projected）做 `FULL OUTER JOIN`，五组指标逐一比较 |
| `outbox_delivery` | 已发布事件必须在宽限期内拿到消费回执 | `outbox_events LEFT JOIN consumed_events`，`published_at < now() - grace` 且无回执即差异 |

`Runner.Run` 遇到某条检查出错会记录并继续跑完其它检查——对账的价值是"一次性看清所有问题"，
而不是第一个错误就停下；首个错误仍会返回给 CLI，用来区分"数据有差异"和"任务失败"。

## 真实数据上抓到的两件事

1. **口径不一致（第一次运行就报了 2 条差异）**。检查最初用"净额"（排除已退款订单），
   投影记的是"毛额 + 单独记退款"，两边天然对不上。排查后确认是检查写错，于是把口径固化成
   ADR-017：`order_count / units_sold / gmv` = 曾支付过的全部订单（含后来退款的），
   `refunded_orders / refunded_amount` = 按**退款发生日**归集，净额由使用方相减得到。
   这段教训写在 `internal/reconcile/repository.go` 的检查注释里：
   **对账口径必须与投影口径逐字一致，否则对账本身就是噪音源。**
2. **历史脏投影**。早期演练时退款事件还没接入，退款没有进投影，对账把它抓了出来；
   用 `cmd/reconcile --rebuild` 从事实重建后账实恢复一致。这说明对账的价值不只是"发现问题"，
   还在于它把问题落到了一条可执行的修复路径上。

## 重放：为什么"没有回执"可以安全地重放

消费侧的回执与业务投影在**同一个事务**里写入，因此"已发布但没有 `consumed_events` 记录"
等价于"投影从未生效"。`Runner.ReplayMissingReceipts` 据此把超过宽限期的事件
（`MissingReceipts`）重新放回 `PENDING`（`RequeueEvents`，`event_id` 不变），
中继会重新投递，而消费侧仍按原 `event_id` 去重——重放不会造成二次投影。
这条路径覆盖的是 broker 侧真的丢数据（例如 Redis 未开 AOF 就重启）：
**我们不假设 broker 不丢，而是让丢失可被发现、可被修复。**

## 读模型重建：CQRS 给的那条退路

`RebuildProjection` 在一个事务里 `DELETE FROM author_sales_daily` 再从
`orders` / `order_items` / `refunds` 事实重新聚合写入，口径与投影逐字相同，因此可重复执行、
结果幂等。这是 CQRS 最关键的性质：**读模型只是事实的一种物化形式，随时可以丢弃并重建**。
所以"投影漂移"是可修复的运维问题，而不是数据损坏——前提是事实侧
（订单、退款单、库存流水）始终是权威。它只应在确认"事实可信、投影漂移"之后使用，
因为它会抹掉投影里任何事实侧没有的内容。

## 退出码契约：把"数据不一致"和"任务失败"分开

`cmd/reconcile` 的注释把契约写死：**0 = 账实一致，1 = 发现差异，2 = 执行失败**
（配置/连库/查询错误）。0 与 1 都能让 cron 与告警系统区分
"需要人看数据"和"任务本身挂了"，否则运维看到红就以为程序坏了。
`--replay` / `--rebuild` 的执行顺序是先补偿再对账：报告要反映"修完之后"的状态，
否则运维看到的是已经处理过的差异，无法判断这次运行到底有没有把问题收敛掉。

## 真实依赖实验（`internal/reconcile/integration_test.go`）

`TestReconcileDetectsDrift` 的验收方式很直接：先证明**不误报**，再逐项注入脏数据证明**能抓到**。

1. `clean data reports no differences`：造一份内部完全一致的业务数据
   （商品/SKU/库存/订单/支付/退款/流水/事件/投影全对齐，库存按"期初 0 + ADJUST +10 - 下单 1"
   由流水建立），对账必须零差异——**只会报警不会分辨的对账没有价值，只会说 OK 的更危险**；
2. `inventory balance drift is detected`：直接 `UPDATE inventory SET quantity = quantity + 7`
   而不写流水（模拟人改库或补偿脚本漏写），`inventory_balance` 必须报差异；
3. `projection drift is detected`：把投影 GMV 人为 +100，报告里直接给出
   `live_gmv = 200` 与 `projected_gmv = 300` 两列，证明差异行能定位到"差在哪一侧"；
4. `refund consistency drift is detected`：把订单改成 REFUNDED 却不改退款单，
   `refund_order_consistency` 必须报差异——状态与售后单据必须互为证据；
5. `missing consumer receipt is detected and replayed`：插一条
   `published_at` 在 10 分钟前、没有回执的 PUBLISHED 事件（模拟 broker 丢数据），
   检查报差异 → `ReplayMissingReceipts` 返回 1 → 事件回到 PENDING → 清理后重新对账恢复一致。

边界说明：注入的库存/投影/退款三类差异在这条用例里通过回退注入恢复；
生产里的投影修复路径是 `--rebuild`，重建逻辑本身没有在集成测试里单独覆盖。

## 已知边界

- 对账只读事实库，不做任何"自动改数"；唯一的写操作是把无回执事件放回 Outbox（幂等）。
- 检查覆盖的是同库不变量（库存/订单/支付/退款/投影/事件），不覆盖外部系统（真实支付渠道、对象存储）。
- `author_sales_projection` 只覆盖"带货订单"（`source_author_id IS NOT NULL`）的口径。
- 差异行上限 50 是刻意的报告可读性取舍；超出的差异需要人工写 SQL 排查。
