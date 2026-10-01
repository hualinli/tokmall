# 优惠券域：把"券"这个跨模块特性做对需要哪几层

## 这一步交付了什么

v1 满减券：商家发券 → 用户领券 → 带券下单 → 支付 → 退款退券 → 对账。
命令入口在 `internal/coupon/handler.go`：

```text
POST /api/v1/coupons                      商家发行券
GET  /api/v1/coupons/mine                 商家查看发行量与进度
GET  /api/v1/coupons/available            可领取列表（匿名可读）
POST /api/v1/coupons/:id/claim            领券
GET  /api/v1/me/coupons[?status=]         我的券
GET  /api/v1/me/coupons/:id               单张持券（下单前校验用）
POST /api/v1/orders                       下单时可带 user_coupon_id
```

## 为什么券值得单独做一轮设计

券本身不复杂，复杂的是它**横跨五个关注点**：

1. **券模板**（发行量、限领、有效期）——营销资源；
2. **持券**（谁领了、用没用、用在哪张订单）——用户资产；
3. **订单金额**（原价 / 优惠 / 实付）——资金事实；
4. **退款**（券要不要退、过期了怎么办）——逆向流程；
5. **对账**（发行量守恒、券与订单一致）——可证明性。

任何一处规则写两遍，就会漂移。同类项目（DomTok）的券最后出现
"`discount_amount` 字段装的是应付金额、订单应付金额恒 0、券可无限领无限用"，
根因就是规则散落在算价、创建、领取三处，且金额字段没有约束。

## 核心不变量

1. **发行量守恒**：`coupons.issued_quantity = count(user_coupons)`，且不超过 `total_quantity`
   （`CHECK (total_quantity = 0 OR issued_quantity <= total_quantity)`）。
2. **每人限领**：同一用户对同一券的持券数 ≤ `per_user_limit`。
3. **一券一单**：`user_coupons.status='USED'` ⟺ `used_at` 与 `order_id` 都非空；
   一张券最多核销到一张订单（`idx_orders_user_coupon_unique` 部分唯一索引）。
4. **金额恒等**：`orders.total_amount = original_amount - discount_amount`，且都为正
   （`orders_amount_split_check`）。
5. **规则唯一**：能不能用、减多少，只由 `coupon.DiscountFor` 决定。

## 并发：为什么必须锁券行

领取要同时满足"不超发"和"不超领"，这是两个不同粒度的约束：

- 不超发是**券级别**的（所有领取者竞争同一个计数器）；
- 不超领是**用户×券**级别的（同一用户的多次领取互相竞争）。

如果只做条件更新（`issued < total`），不超发没问题，但"同一用户并发领 20 次"
会因为计数检查与插入之间没有互斥而超领。如果只锁用户行，不同用户之间的发行量竞争
又没有串行化。

`GormRepository.Claim` 的做法是：**第一步就 `SELECT ... FROM coupons WHERE id=? FOR UPDATE`**，
把整段"校验状态/有效期 → 条件更新发行量 → 计数校验限领 → 写入持券"放进同一把券行锁里。
代价是同一张券的领取被串行化（对营销活动来说这正是想要的：抢券本来就是串行的），
收益是两类约束同时成立，且实现只有一处。

第二道防线是条件更新 + 数据库 CHECK：即使将来有人绕过锁，发行量也不会越过上限。

## 真实 PostgreSQL 实验

```bash
TOKMALL_RUN_PG_INTEGRATION=1 TOKMALL_TEST_DATABASE_DSN='...' \
  go test ./internal/coupon -run TestPostgresCouponClaimConcurrency -v
```

| 场景 | 断言 |
|---|---|
| 100 用户并发抢 10 张 | 恰好 10 成功、90 售罄；`issued_quantity` == 持券行数 == 10 |
| 同一用户并发领 20 次（限领 2） | 恰好 2 成功、18 次 `COUPON_ALREADY_CLAIMED`；**被拒的领取回滚了发行量计数** |
| 绕过应用层裸 UPDATE 推高发行量 | 被 `coupons_issued_not_exceed_total_check` 拒绝 |

订单侧（`internal/order/coupon_postgres_integration_test.go`）：金额拆分、幂等重放不二次核销、
退款退券（有效期内 UNUSED / 过期 EXPIRED）、门槛不足 / 别人的券 / 重复用券全部被拒。

## 两个设计细节

**（1）过期是时间的函数，不需要定时任务。**
`user_coupons.status` 只在**核销/退券**时落库（UNUSED ↔ USED/EXPIRED）；
"券现在过期没有"在读取时用 `valid_to` 与当前时间比较得出。
这样省掉一个定时任务，也不会出现"任务没跑导致过期券还能用"的窗口。
代价是"过期券数量"这类统计要走 SQL 判断而不是查状态列。

**（2）退款退券是"营销资源"的逆向流程，与资金无关。**
退款成功后券退回 UNUSED（过期则 EXPIRED），并清空订单引用；
这一步失败不会回滚退款——退款是资金终态，优先级高于券。
代码里对"券不存在""券不是被本单用掉的"都只记 Warn 并继续，
留给对账去暴露（`coupon_usage` 检查覆盖）。

## 对账：约束挡不住的部分才需要检查

新增三项检查后 `bin/reconcile` 从 6 项变 9 项：

- `coupon_issuance`：发行量 = 持券数，且不超上限；
- `coupon_usage`：已核销的券必须对应订单，且订单优惠金额 = 券面额；
- `order_amount_split`：`total = original - discount`，且用券订单必须引用持券。

有意思的是写测试时发现：另外两种更脏的形态（"已核销却没有订单"、"实付 ≠ 原价 − 优惠"）
**被数据库 CHECK 直接挡住了，注入不进去**。这说明约束把"能写坏的方式"收窄了，
对账只需要覆盖约束管不到的部分（跨表关系）。

## 已知边界

- 只支持全场满减；不支持折扣券、品类/单品券、券叠加、平台券；
- 门槛比较的是商品金额（不含运费——本项目 v1 没有运费）；
- 券不能转赠；没有券的"退回次数"概念（一次退款退一张，v1 每人限领 1~N 张各自独立）。
