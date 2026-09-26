# 订单数据模型：先固定交易事实，再实现下单流程

> 阶段 1A 的实施记录。“本阶段暂不实现”指当时交付边界；后续创建订单、支付、超时关单和履约现已实现，当前状态见 [Commerce v1 技术实现](../commerce-v1-technical-design.md)。

## 1. 这一阶段解决什么问题

订单不是把商品表复制一遍，也不是几个 CRUD 接口。它首先要回答：价格变化、商品下架、视频删除、客户端重试、支付失败重试、多个关单 Worker 并发时，哪些数据仍然可信。

本阶段只建立四张表和对应领域类型：

- `orders`：订单身份、买家、商家、状态、金额、幂等信息、归因和状态时间；
- `order_items`：成交时的商品、SKU、商家、标题、规格、单价和数量快照；
- `payments`：一次次支付尝试；
- `video_products`：视频与商品的受控推广关系。

暂不实现下单事务、库存扣减、支付状态机或 HTTP 接口。这样先把数据库不变量审清楚，后续并发代码不会建立在含糊的数据模型上。

## 2. 为什么订单项必须保存快照

商品标题、SKU 规格、价格和商家信息都是可变数据，但订单是历史交易事实。若订单详情每次回查当前商品表，会出现：用户下单 99 元，商家改价后历史订单却显示 129 元；SKU 改名后发票或客服记录也随之改变。

因此 `order_items` 同时保存两类信息：

- `product_id`、`sku_id`：用于追踪、对账和定位来源；
- `merchant_id_snapshot`、`product_title_snapshot`、`sku_spec_snapshot`、`unit_price`、`quantity`、`line_amount`：用于还原成交事实。

展示历史订单和核对金额使用快照，不依赖商品当前状态。Commerce v1 只支持单 SKU，所以数据库用 `UNIQUE(order_id)` 明确限制一单一项；以后支持购物车时用新迁移删除该约束，而不是现在提前留下未经测试的多商品语义。

## 3. 金额为什么用整数

`total_amount`、`unit_price`、`line_amount` 和支付金额都使用 `BIGINT`，Go 中用 `Amount int64`，单位为分。浮点数无法精确表示多数十进制小数，跨 JSON、Go 和 PostgreSQL 计算时可能产生不可解释的舍入差异。

数据库约束金额必须大于零。后续下单服务还必须用服务端读取的 SKU 单价计算，并校验：

```text
line_amount = unit_price × quantity
total_amount = 所有 line_amount 之和
```

数据库没有直接写乘法 CHECK，是为了避免整数溢出等表达式边界被误当成业务校验完成；计算、溢出检测和总额一致性由创建订单事务负责。

## 4. 状态与时间如何表达

订单状态预留完整普通订单生命周期：

```text
PENDING_PAYMENT → PAID → FULFILLING → COMPLETED
PENDING_PAYMENT → CANCELED
PENDING_PAYMENT → EXPIRED
```

表中的 CHECK 只约束状态值合法，不代表任意状态可以互转。后续状态机仍须使用 `WHERE id=? AND status=?` 的条件更新，把“来源状态”放进原子写条件。

数据库还约束状态与事实时间一致：只有 `PAID/FULFILLING/COMPLETED` 可以且必须存在 `paid_at`，只有 `CANCELED` 存在 `canceled_at`，只有 `EXPIRED` 存在 `expired_at`；`expires_at` 必须晚于创建时间。这些是单行内可表达的不变量，不能只寄希望于每条写路径都记得维护。

时间字段区分计划和事实：

- `expires_at`：支付截止时间，创建订单时确定，供超时 Worker 扫描；
- `paid_at`、`canceled_at`、`expired_at`：相应状态真正发生的时间；
- `created_at`、`updated_at`：记录生命周期。

这些时间不能互相替代。例如 `expires_at` 到达只表示“可以尝试过期”，支付和过期并发时仍需由数据库状态条件仲裁。

## 5. 幂等如何落到数据模型

订单的幂等范围是 `(user_id, idempotency_key)`，避免不同用户碰巧使用相同客户端 key 时互相冲突。只存 key 仍不够：同一个 key 可能被错误复用到不同 SKU、数量或来源。因此订单还保存 SHA-256 十六进制 `request_fingerprint`，数据库要求它是 64 位小写十六进制字符串。

后续创建订单时：

- 同 key、同 fingerprint：返回原订单，不再扣库存；
- 同 key、不同 fingerprint：返回幂等冲突；
- 并发首次请求：唯一约束由 PostgreSQL 仲裁，事务层保证只生成一个订单和一次库存变更。

## 6. 支付为什么是一对多尝试

支付表没有 `UNIQUE(order_id)`。一笔订单可能第一次支付失败，用户更换渠道或重新发起第二次尝试；把支付做成订单的一对一扩展会丢失失败轨迹，也阻塞合理重试。

每次尝试由内部 `payment_no` 唯一标识；真实接入渠道后，非空 `provider_trade_no` 也通过部分唯一索引防止同一第三方交易重复入账。支付 `SUCCESS` 时必须有 `paid_at`，其他状态不得伪造成功时间。

注意：允许多次尝试不等于允许多次成功。后续支付事务必须由订单的 `PENDING_PAYMENT → PAID` 条件更新做最终仲裁。

## 7. 索引是围绕访问路径设计的

- `(user_id, status, created_at DESC, id DESC)`：用户订单列表和稳定游标分页；
- 待支付部分索引 `(expires_at, id) WHERE status='PENDING_PAYMENT'`：超时 Worker 扫描，后续配合 `FOR UPDATE SKIP LOCKED`；
- 已支付来源作者/视频部分索引：支撑带货订单和 GMV 聚合，同时不索引无来源订单；
- `video_products(video_id, sort_order, id) WHERE status='ACTIVE'`：Feed 一页视频批量装配商品卡；
- `payments(order_id, created_at DESC, id DESC)`：查看支付尝试历史。

索引没有为所有字段“保险式添加”，因为每个索引都会增加写放大和维护成本。

## 8. 删除与外键策略

外键按“在线关系”和“历史事实”区别处理：

- `video_products.video_id` 使用 `ON DELETE CASCADE`：视频硬删后在线推广关系应一并消失；
- `video_products.author_id/product_id` 使用默认 RESTRICT：正常流程通过软删除、下架或禁用维护关系；
- `orders.user_id`、`order_items.order_id/product_id/sku_id`、`payments.order_id` 使用默认 RESTRICT：禁止物理删除仍被交易事实引用的数据；
- `orders.source_*` 刻意不设外键：它们是下单归因快照。来源视频即使硬删、作者即使注销、商品即使未来归档，订单和统计口径仍然保留。

商品标题、规格和商家等可读历史信息已存在订单项快照中，所以来源对象消失不会让订单详情残缺。

## 9. 面试时怎样说明边界

当前设计有意保持克制：

- 单币种、金额单位固定为人民币分；多币种需要增加 currency 并定义汇率和舍入规则；
- 单 SKU 订单；购物车、多商家拆单需要删除单项唯一约束并引入父子订单；
- 没有优惠、税费、运费和退款；这些都会改变金额不变量和状态机；
- 没有真实支付渠道，只为模拟支付和未来回调预留多次尝试模型；
- `video_products.author_id` 与视频作者一致、挂载商品在售等跨表规则无法用普通 CHECK 表达，由后续 service 在事务内校验；
- 本阶段的迁移测试验证 DDL 契约，不代替后续在真实 PostgreSQL 上执行迁移、事务和并发实验。

核心原则是：先用数据库约束固定局部不变量，再由事务和条件更新处理跨表、并发和状态迁移；不要用“以后在 service 判断”代替所有数据库保护。
