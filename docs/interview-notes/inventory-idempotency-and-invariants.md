# 库存幂等与数据库不变量：面试讲解

## 先说结论

TokMall 的人工调库存不是“更新一个 quantity 字段”的 CRUD。它要同时保证三件事：

1. 同一个请求重试不会重复改变库存；
2. 不同业务载荷误用同一个幂等键时不会被伪装成成功；
3. 库存余额和库存流水要么一起提交，要么一起回滚，任何时刻都能对账。

当前实现选择 PostgreSQL 作为库存事实源，用数据库唯一约束完成并发仲裁，用条件更新防止负库存，用短事务把幂等 claim、库存变更和流水绑定在一起。

## 1. 为什么不能让 `biz_id` 全局唯一

客户端生成的幂等键通常只保证在一次业务操作中唯一，不保证跨 SKU、跨商家、跨操作全局唯一。例如两个 SKU 的商家请求都使用 `batch-20260916`：

```text
SKU 11 + ADJUST + batch-20260916
SKU 12 + ADJUST + batch-20260916
```

如果表上只有 `UNIQUE (biz_id)`，第二笔请求会撞到第一笔流水。服务层若看到唯一键冲突就直接返回当前库存，就会产生“假成功”：第二个 SKU 实际没有入库，却被当作第一次请求的重放。

因此人工调整的唯一范围是：

```text
(sku_id, reason=ADJUST, client_idempotency_key)
```

数据库对应 `UNIQUE (sku_id, reason, biz_id)`。`merchant_id` 不再冗余写进流水：SKU 通过 `skus -> products -> merchant_id` 的归属链绑定到商家，写入前的 ownership check 保证调用者只能操作自己的 SKU。这样既避免跨 SKU 误合并，也避免把商家字段复制到库存事实表后出现双写不一致。

接口会先去掉 key 两端空白，并把长度限制为 128 字节，避免无限长客户端输入进入唯一索引和日志；OpenAPI 契约同步声明这一边界。

`reason` 是幂等命名空间的一部分。人工调整使用 `ADJUST`；订单扣减和订单取消使用不同的 reason，并由各自的业务号组成 biz id。相同命名空间内，幂等键只能代表一组固定业务事实。

## 2. 唯一键冲突不等于幂等成功

服务层会比较冲突流水和本次请求的业务载荷：

```text
sku_id、change、reason、biz_id、order_id（若有）
```

语义固定为：

| 情况 | 结果 |
|---|---|
| 同一 SKU、同一操作、同一 key、载荷完全相同 | 返回当前库存，视为安全重放 |
| 同一范围的 key 被不同 change 复用 | 409 `IDEMPOTENCY_CONFLICT` |
| 同一范围的 key 指向不同目标/操作 | 409 `IDEMPOTENCY_CONFLICT` |
| 同一个 key 用在另一个 SKU | 独立的库存操作，不冲突 |

只比较 key 不够，因为客户端可能因为 bug、重试代码复用或恶意请求把同一个 key 搭配不同数量提交。若把所有唯一冲突都当作成功，库存调整就失去了幂等保护的安全边界。

## 3. 事务为什么先 claim，再条件更新

`Adjust` 在一个短 PostgreSQL 事务里执行两步核心动作：

```text
1. INSERT inventory_transactions ... ON CONFLICT DO NOTHING
   └─ 抢占 (sku_id, reason, biz_id) 这组幂等 claim
2. UPDATE inventory
   SET quantity = quantity + change, version = version + 1
   WHERE sku_id = ? AND quantity + change >= 0
```

成功插入流水时，事务继续做库存条件更新；如果库存不足或库存行不存在，事务回滚，前面的 claim 也一起消失。只有两步都成功，事务才提交。因此不会出现“流水写了但库存没扣”或“库存改了但没有流水”。

先 claim 的原因是让并发重放在库存更新前就被唯一约束仲裁：

```text
请求 A：claim 成功 -> 更新库存 -> 提交
请求 B：相同 key 等待唯一约束 -> ON CONFLICT 0 行 -> 读已有流水 -> 不更新库存
```

请求 B 必须再做载荷比较。相同载荷返回重放成功，不同载荷返回幂等冲突。这里依赖的是数据库唯一索引的并发语义，而不是“先查再写”的时间窗口；先查再写会让两个并发请求都看到不存在，最终重复扣减。

库存更新使用：

```sql
UPDATE inventory
SET quantity = quantity + :change,
    version = version + 1,
    updated_at = now()
WHERE sku_id = :sku_id
  AND quantity + :change >= 0;
```

`RowsAffected = 0` 时再区分“库存不足”和“库存行缺失”。库存行缺失不是一次可自动修复的入库，而是数据库不变量被破坏，应作为服务端异常处理，不能继续惰性创建来掩盖问题。

## 4. SKU 与库存行为什么必须原子创建

早期实现允许先创建 SKU，第一次正向调库存时再惰性创建 inventory 行。这个设计有两个问题：

- 两个并发首调可能同时发现缺行并竞争插入同一个主键；
- SKU 已经对外可见，但库存事实尚未建立，读写路径对“缺行”的解释不一致。

现在 `product.CreateSKU` 在同一个数据库事务中完成：

```text
INSERT INTO skus (...)
INSERT INTO inventory (sku_id, quantity=0, version=0)
COMMIT
```

第二条写入使用原始 SQL，而不是 import inventory 模块的 model。这样保持模块不共享 Go 类型，同时把 SKU 生命周期所需的跨表不变量放在数据库适配层完成；库存模块仍然拥有库存变动、流水和并发扣减语义。未来如果交易域进一步合并，可以把这段编排上移到 commerce application service，但“同库同事务”的边界不能退化成两个独立请求。

迁移 000011 会给历史 SKU 补建零库存行，之后新 SKU 不再允许走惰性初始化路径。

## 5. 为什么 CHECK 不能只放在 service

service 校验是用户体验层的快速反馈，但数据库可能还有其他写入口：

- 订单或关单 Worker；
- 未来的 MQ consumer；
- 运维修复脚本；
- 数据导入和回放任务；
- 测试或临时管理工具。

因此迁移恢复以下核心 CHECK：

- `inventory.quantity >= 0`；
- `products.status` 只能是 `ON_SALE/OFF_SALE/DELETED`；
- `skus.status` 只能是 `ACTIVE/DISABLED`；
- `skus.price > 0`；
- 商品商家 ID 为正、商品标题非空；
- 流水变更量不能为零，reason 必须是已知业务类型。

应用层仍保留参数校验和状态迁移校验，因为它能返回更友好的业务错误；数据库 CHECK 是所有写入口共享的最后一道防线。两层职责不同，不能用 service 校验替代数据库约束。

## 6. 迁移与回滚边界

000011 是前向迁移，不修改 000007~000010 历史文件。迁移顺序是：

1. 为历史缺行 SKU 补建零库存；
2. 删除旧的全局 `biz_id` 唯一约束；
3. 建立 `(sku_id, reason, biz_id)` 唯一约束；
4. 恢复核心 CHECK。

约束校验失败时迁移应失败并暴露已有脏数据，而不是静默修改交易事实。数据库迁移在 PostgreSQL 中按迁移事务执行，失败会整体回滚，避免只建立了一半的约束。

down 会删除 000011 新增的 CHECK 和复合唯一约束，并尝试恢复 000007 的全局唯一约束。它不会删除迁移后产生的库存行：删除这些行会破坏真实库存事实，不能把“回滚 schema”误做成“删除业务数据”。如果降级前已经存在跨 SKU 重复的 biz id，恢复旧全局唯一约束会有数据冲突；这属于降级前必须先检查和处理的数据条件，不应在 down 中静默删数据。

## 7. 面试时可以这样回答

**问：你怎么防止库存超卖？**

> 当前普通库存调整使用 PostgreSQL 条件更新：`UPDATE ... WHERE quantity + change >= 0`，数据库行锁负责并发串行化，`RowsAffected=0` 表示库存不足。更新和库存流水在同一短事务里，流水还能用于对账。SKU 创建时库存行已经在同一事务中初始化，因此不会用惰性创建掩盖缺失数据。秒杀或热点 SKU 的吞吐问题再考虑 Redis 预扣和异步确认，但那是性能演进，不是当前正确性的前提。

**问：幂等键撞了为什么不能直接返回成功？**

> 唯一键只能说明 key 已经被使用，不能说明本次请求和原请求相同。我们按 SKU、操作 reason 和客户端 key 定义范围，冲突后读取已有流水并比较 change、目标和操作等业务事实；同载荷才重放成功，不同载荷返回 409 `IDEMPOTENCY_CONFLICT`。数据库唯一索引负责并发仲裁，service 负责业务语义。

**问：为什么不在第一次调库存时创建 inventory 行？**

> 惰性创建会产生首调竞态，也让 SKU 可见性和库存事实分离。现在 SKU 和零库存行在同一数据库事务中创建；如果第二条写失败，SKU 也回滚。模块之间不共享 model，product repository 只执行初始化所需的表级写入，库存变动仍由 inventory 模块负责。

**问：CHECK 和代码校验重复吗？**

> 不重复。service 校验面向 API 交互和业务错误，CHECK 保护数据库不变量，防止 Worker、脚本和未来消费者绕过 service 写出负库存或非法状态。所有写入口都必须服从数据库底线。
