# Transactional Outbox 与至少一次投递（Redis Streams）

## 这一步交付了什么

支付成功之后有一串副作用：作者成交统计、通知、风控、对账。它们不能和支付放在同一个本地事务里，
也不能"提交订单后再发消息"。这一步交付的是可靠分发的全套机制：

- `migrations/000017_outbox_and_projection.up.sql`：`outbox_events`、`consumed_events`、
  `author_sales_daily`（事件驱动的作者日销读模型）；
- `internal/outbox`：仓储（`SKIP LOCKED` + 租约）、中继（退避/DLQ）、传输抽象
  （`Publisher`，默认 Redis Streams）、消费组消费者（回执 + 投影同事务）；
- `internal/attribution/projector.go` 作者日销投影；`cmd/outboxctl` 运维命令。

## 为什么"提交后再发消息"一定丢事件

`internal/outbox/model.go` 的包注释把两个否决方案写死了：

1. 把副作用塞进支付事务——通知、统计、风控是**跨系统**的，本地事务管不到它们，
   硬凑就是分布式事务（ADR-016 明确不做）；
2. 先提交、后发消息——进程在"提交成功"与"消息发出"之间崩溃，事件永久消失，
   而订单已经是 PAID，事实与下游永久分叉。

Outbox 把"要发的消息"当作业务数据写进同一个事务：事务提交则事件一定在表里，
事务回滚则事件也一起消失。投递是之后异步发生的事。

## 同事务入队：唯一的写入入口

`outbox.Enqueue(tx *gorm.DB, event Event)` 只接受事务句柄，传 `nil` 直接报错
（`internal/outbox/repository.go`）；`EnqueueOrderPaid` / `EnqueueOrderRefunded` 把事件类型、
聚合类型、分区键的约定收在一处，业务侧调用点只有支付事务（`internal/order/repository.go`）
与退款事务（`internal/order/refund_repository.go`）。

`partition_key` 默认 `order`，有来源作者时用 `author:<id>`：作者日销投影必须按作者串行。
事件类型带版本号（`commerce.order.paid.v1`），语义变更时新增版本，而不是让老消费者解析新字段。

## 中继：SKIP LOCKED + 租约 + 指数退避 + DLQ

`GormRepository.FetchPending` 在一个短事务里取批并加租约：

```sql
SELECT ... WHERE status = 'PENDING' AND available_at <= now()
ORDER BY id LIMIT n FOR UPDATE SKIP LOCKED
-- 同事务：leased_until = now() + lease, attempts = attempts + 1
```

用租约而不是"取走即删除"：中继崩溃时事件仍在表里，租约到期后被下一个实例重新取走——这就是至少
一次投递的来源，也让多实例并行安全（`SKIP LOCKED` 让实例之间不互相阻塞）。

`Relay.RunOnce` 的循环是"先投递、后标记"：投递成功的 ID 收集起来一次性 `MarkPublished`；
如果投递成功而标记失败，事件会在租约过期后重投，日志里明确写着
"mark outbox published failed; events will be redelivered"。

失败路径（`handleFailure`）按尝试次数分流：`attempts >= maxTries` 时 `MarkDead` 进 DLQ
（不删除、不静默丢弃，等人工判断是重放还是作废）；否则 `MarkRetry` 用指数退避
1s→2s→4s…，上限 1 分钟——上限存在的意义是让"broker 恢复"能在可接受时间内被自动发现。
`Run` 每轮处理到取不满一批为止，积压时不会白等一个 tick。

默认配置（`internal/config/config.go`）：tick 1s、批量 50、租约 30s、最大尝试 5 次、
流 `tokmall:events:commerce.v1`、`MAXLEN` 近似裁剪 10000。

## 为什么投递语义是"至少一次"

"先投递、后标记"之间存在崩溃窗口，重投必然发生；反过来"先标记、后投递"会真丢事件。
做到精确一次需要分布式事务或 broker 事务，代价远高于让消费方做一次幂等去重（`consumed_events`
主键）。这是一个**刻意的**取舍：语义写在接口注释里，去重责任写在 `Handler` 的契约里。

## 消费侧：回执与投影在同一个事务

`Consumer.process`（`internal/outbox/consumer.go`）是幂等消费的全部秘密：

```text
BEGIN
  INSERT INTO consumed_events (consumer, event_id, ...) ON CONFLICT DO NOTHING
  -- RowsAffected == 0 → 已处理过：跳过业务写入（duplicated）
  handler.Handle(tx, event)          -- 业务投影，复用同一 tx
COMMIT
XACK                                  -- 只有提交成功才确认
```

于是有了一条可依赖的等价关系：**没有回执 ⟺ 投影没有生效**——它后来成了对账补偿的依据。
处理失败不 ACK，消息留在 PEL 等认领重投或进 DLQ；无法解析的消息（缺 event_id、payload 坏了）
不可能靠重试修复，直接投 DLQ 并 ACK，避免卡住消费组；未订阅的类型确认掉但不处理，否则会无限重投。

崩溃兜底是 `XAUTOCLAIM`：`ClaimOnce` 认领"投递出去超过 `claimIdle` 仍未 ACK"的消息，
用 `XPENDING` 读投递次数；超过 `maxDeliveries` 的消息投到 DLQ Stream 并 ACK 原消息——
原流不再积压，但事件本身没丢（DLQ 里有完整载荷，可人工重放）。`XINFO GROUPS` 的 `pending`
被导出成消费延迟指标。

## 运维入口：为什么是命令行而不是管理接口

`cmd/outboxctl` 提供 `status`（pending/dead/最老待投递事件年龄）、`dlq list --limit`、
`dlq requeue <event_id>... | --all`、`drain`（手动投递一批）。理由写在文件头：低频且需要审计的
运维动作放在 HTTP 上只会扩大攻击面，而且它们必须在应用已经挂掉时仍可用。`Requeue` 重置为
PENDING 但 **`event_id` 不变**，消费侧仍按原幂等键去重；`RowsAffected = 0` 时还会区分
"事件不存在"（`ErrEventNotFound`）和"存在但不是 DEAD"（`ErrEventNotDead`）。`drain` 复用生产
同一套中继逻辑，不产生第二条投递路径。

## 为什么默认 Redis Streams 而不是 Kafka（ADR-014）

需要的能力是"至少一次 + 消费组 + 崩溃后重投 + 可观测积压"，Streams 用
`XREADGROUP`/`XACK`/`PEL`/`XAUTOCLAIM` 全部具备，而项目本来就依赖 Redis。
**代价必须写在明面上**：Redis 持久化弱于 Kafka（AOF everysec 仍有秒级丢失窗口）、没有跨机房复制、
内存需要 `MAXLEN` 近似裁剪。因此本项目不把"事件不丢"押在 broker 上，而是用 Outbox + 消费回执 +
对账重放兜底（见 `reconciliation-and-projection-rebuild.md`）。传输层是接口 `outbox.Publisher`，
换 Kafka 只需替换实现，事件语义、幂等键、DLQ 与重放流程不变。

## 真实依赖实验（`internal/outbox/integration_test.go`）

```bash
TOKMALL_RUN_PG_INTEGRATION=1 TOKMALL_RUN_REDIS_INTEGRATION=1 \
TOKMALL_TEST_DATABASE_DSN='host=127.0.0.1 user=tokmall password=tokmall dbname=tokmall port=5432 sslmode=disable' \
go test ./internal/outbox -count=1 -v
```

`TestOutboxEndToEnd` 用真实 PostgreSQL + 真实 Redis（每条用例独立 stream/group）验证六个场景：

1. **paid event is projected exactly once**：一条事件消费一次，投影累加正确，`outbox_events` 标记 PUBLISHED、`consumed_events` 只有一条回执；
2. **duplicate delivery does not double count**：手工把同一 `event_id` 重新投进流（模拟"投递成功但标记失败"），订单数与 GMV 不重复累加；
3. **refund event offsets the projection**：毛额保持、退款按发生日单独累加，两者分列；
4. **business rollback drops the event with it**：事务内入队后返回错误，事件、投影都不存在；
5. **publish failure retries then dead letters and can be replayed**：永远失败的投递器把事件推成 DEAD，投影不受影响；`Requeue` 后正常投递并消费；
6. **stale message is reclaimed after consumer crash**：另一个消费者读走不 ACK 后"崩溃"，新消费者用 0 idle 认领并处理成功。

## 端到端演练（`scripts/drill_commerce_loop.sh`）

九步 curl + psql 的闭环演练，与 Outbox 有关的验收点在第 6～9 步：支付后 `sleep 2`
给中继与消费者一个投递周期，然后直接查库断言 `outbox_events` 中该订单的事件已 PUBLISHED、
不存在 PENDING 积压、作者日销投影的 GMV 已累加、`consumed_events` 有该订单的回执；
退款后断言订单状态、库存回补与投影的 `refunded_amount`；最后从 `/metrics` 抓
`outbox_pending_events` / `outbox_dead_events` / `outbox_published_total` /
`outbox_consumer_processed_total` / `outbox_consumer_duplicate_total`。脚本的意义是
"任何一步失败都打印响应原文"，可以在面试现场或故障演练时当场看到链路。

## 已知边界

- 下游是最终一致（秒级延迟），不是强一致；读模型落后于事实是正常状态。
- 事件保留期取决于 Redis `MAXLEN` 与内存预算，超出窗口的历史只能靠读模型重建/对账。
- DLQ 没有自动重放策略，`outboxctl dlq requeue --all` 由人触发；未订阅的事件类型会被 ACK 丢弃。
