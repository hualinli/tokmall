# Commerce v1 技术设计

> 状态：技术设计草案（未实现）
> 关联文档：
> - 业务梳理：[docs/commerce-business.md](./commerce-business.md)
> - 总作战手册：[docs/content-commerce-backend-agent-guide.md](./content-commerce-backend-agent-guide.md)
> - Feed 补充设计：[docs/feed-system-supplement.md](./feed-system-supplement.md)

## 1. 目标与范围

### 1.1 目标

在现有 TokMall 模块化单体中，实现“视频带货 -> 普通下单 -> 作者归因统计”的最小交易闭环。

### 1.2 范围内

- 商品 / SKU / 库存
- 视频挂商品（`video_products`）
- 商品详情与商品列表
- 普通下单
- 模拟支付
- 订单状态机
- 订单超时关闭与库存回补
- 订单归因（作者 / 视频 / 商品）
- 作者带货统计（订单数、购买人数、GMV）
- OpenAPI 契约与 route-sync

### 1.3 范围外（本设计不实现）

- 秒杀活动
- 购物车
- 优惠券
- 售后 / 退款
- 佣金结算
- 多商家 / 店铺体系
- Kafka / 异步消息队列
- 微服务拆分
- 真实支付渠道

## 2. 总体架构

### 2.1 模块划分

沿用现有模块化单体风格，新增以下业务模块：

```text
internal/
  product/        商品、SKU、商品上下架
  inventory/      库存、库存流水、库存回补
  order/          订单、订单项、订单状态机、模拟支付
  attribution/    订单归因、作者带货统计、转化查询
```

模块依赖方向：

```text
order  -> inventory   （扣减/回补库存）
order  -> product     （读取商品价格、状态、SKU）
product -> （无反向依赖 order）
attribution -> order / video_products 的只读模型，不反向依赖业务模块内部类型
feed / video -> product 的只读商品卡接口
```

### 2.2 与现有模块的关系

```text
video 模块
  -> 增加 video_products 关联维护（作者选择商品挂到视频）
feed 模块
  -> 返回商品卡时调用 product 只读查询，不直接读库存/订单
user 模块
  -> 不新增商家逻辑；作者带货数据由 attribution 模块提供
```

### 2.3 关键原则

- 跨模块只传 ID / DTO，不共享 model。
- 交易域内部（product / inventory / order / payment）可以强耦合，但不向内容域泄漏。
- 作者不是商家：`products.merchant_id` 与 `video_products.author_id` 是两个不同概念。
- 订单归因是下单快照，之后不随作者/视频信息变化而改变。

## 3. 数据模型设计

### 3.1 表清单

| 表 | 说明 | 主要写方 | 主要读方 |
|---|---|---|---|
| `products` | 商品基本信息 | product | feed、order、attribution |
| `skus` | SKU 与价格 | product | order、feed（通过 product） |
| `inventory` | 库存数量 | inventory | order |
| `inventory_transactions` | 库存流水，用于对账 | inventory | inventory、attribution |
| `video_products` | 视频与商品的推广关系 | video | feed、attribution |
| `orders` | 订单主表 | order | order、attribution |
| `order_items` | 订单项快照 | order | order |
| `payments` | 模拟支付记录 | order | order |
| `author_sales_stats` | 作者带货聚合统计 | attribution | attribution、user |

### 3.2 核心 DDL 草案

#### products

```sql
CREATE TABLE products (
    id          BIGSERIAL PRIMARY KEY,
    merchant_id BIGINT NOT NULL,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    cover_key   TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'ON_SALE'
                CHECK (status IN ('ON_SALE', 'OFF_SALE', 'DELETED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_products_merchant_status
    ON products (merchant_id, status, id);
```

#### skus

```sql
CREATE TABLE skus (
    id          BIGSERIAL PRIMARY KEY,
    product_id  BIGINT NOT NULL REFERENCES products(id),
    spec        TEXT NOT NULL DEFAULT '',
    price       BIGINT NOT NULL,          -- 单位：分
    status      TEXT NOT NULL DEFAULT 'ACTIVE'
                CHECK (status IN ('ACTIVE', 'DISABLED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_skus_product ON skus (product_id, status, id);
```

#### inventory

```sql
CREATE TABLE inventory (
    sku_id      BIGINT PRIMARY KEY REFERENCES skus(id),
    quantity    BIGINT NOT NULL CHECK (quantity >= 0),
    version     BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### inventory_transactions

```sql
CREATE TABLE inventory_transactions (
    id              BIGSERIAL PRIMARY KEY,
    sku_id          BIGINT NOT NULL REFERENCES skus(id),
    order_id        BIGINT,
    change          BIGINT NOT NULL,      -- 正数=入库，负数=扣减
    reason          TEXT NOT NULL,        -- ORDER_CREATED / ORDER_CANCELED / ORDER_EXPIRED / ADJUST
    biz_id          TEXT NOT NULL,        -- 业务幂等键，如 order_id + reason
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (biz_id)
);
```

#### video_products

```sql
CREATE TABLE video_products (
    id          BIGSERIAL PRIMARY KEY,
    video_id    BIGINT NOT NULL REFERENCES videos(id),
    author_id   BIGINT NOT NULL REFERENCES users(id),
    product_id  BIGINT NOT NULL REFERENCES products(id),
    status      TEXT NOT NULL DEFAULT 'ACTIVE'
                CHECK (status IN ('ACTIVE', 'DISABLED')),
    sort_order  INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (video_id, product_id)
);

CREATE INDEX idx_video_products_author
    ON video_products (author_id, status, id);
```

#### orders

```sql
CREATE TABLE orders (
    id                  BIGSERIAL PRIMARY KEY,
    order_no            TEXT NOT NULL UNIQUE,
    user_id             BIGINT NOT NULL REFERENCES users(id),
    merchant_id         BIGINT NOT NULL,
    total_amount        BIGINT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'PENDING_PAYMENT'
                        CHECK (status IN (
                            'PENDING_PAYMENT',
                            'PAID',
                            'FULFILLING',
                            'COMPLETED',
                            'CANCELED',
                            'EXPIRED'
                        )),
    referrer_author_id  BIGINT,
    referrer_video_id   BIGINT,
    referrer_product_id BIGINT,
    idempotency_key     TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at             TIMESTAMPTZ,
    expired_at          TIMESTAMPTZ,
    UNIQUE (user_id, idempotency_key)
);

CREATE INDEX idx_orders_user_status
    ON orders (user_id, status, id DESC);

CREATE INDEX idx_orders_author
    ON orders (referrer_author_id, created_at DESC);
```

#### order_items

```sql
CREATE TABLE order_items (
    id                  BIGSERIAL PRIMARY KEY,
    order_id            BIGINT NOT NULL REFERENCES orders(id),
    sku_id              BIGINT NOT NULL REFERENCES skus(id),
    product_id          BIGINT NOT NULL REFERENCES products(id),
    product_name_snapshot TEXT NOT NULL,
    price_snapshot      BIGINT NOT NULL,
    quantity            INT NOT NULL CHECK (quantity > 0)
);

CREATE INDEX idx_order_items_order ON order_items (order_id);
```

#### payments

```sql
CREATE TABLE payments (
    id          BIGSERIAL PRIMARY KEY,
    payment_no  TEXT NOT NULL UNIQUE,
    order_id    BIGINT NOT NULL REFERENCES orders(id),
    amount      BIGINT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'PENDING'
                CHECK (status IN ('PENDING', 'SUCCESS', 'FAILED')),
    paid_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (order_id)
);
```

#### author_sales_stats

```sql
CREATE TABLE author_sales_stats (
    author_id    BIGINT PRIMARY KEY REFERENCES users(id),
    order_count  BIGINT NOT NULL DEFAULT 0,
    buyer_count  BIGINT NOT NULL DEFAULT 0,
    gmv          BIGINT NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 3.3 索引与约束要点

- `orders.user_id + idempotency_key` 唯一，保证重复提交不产生多单。
- `inventory_transactions.biz_id` 唯一，保证库存流水幂等。
- `video_products.video_id + product_id` 唯一，防止同一视频重复挂同一商品。
- `orders.referrer_author_id` 建索引，支撑作者维度统计。
- 金额统一用 `BIGINT` 存“分”，避免浮点误差。

### 3.4 对现有表的影响与迁移策略

现有数据库已经有 001~006 号迁移，Commerce v1 全部采用**新增表 + 新增索引**，不修改/不删除现有业务表。

#### 现有 `videos` 表能否满足需求？

结论：**可以继续作为“视频内容表”使用，但商品关联不能直接塞进 `videos`。**

现有 `videos` 表字段：

```text
id, author_id, title, object_key, cover_key,
duration_seconds, status, published_at, created_at, updated_at
```

它已经能表达“视频属于哪个作者、是否发布、发布时间”等 Feed 所需信息。  
但 Commerce 需要的是“一个视频可以挂多个商品、一个商品可以被多个作者/视频推广、按作者统计带货数据”，这属于多对多关系，应该用独立的关联表：

```text
video_products(video_id, author_id, product_id, ...)
```

不建议在 `videos` 上直接加 `product_ids JSONB` 或 `product_id` 单列，原因：

1. 一个视频挂多个商品时，单列无法表达。
2. JSON 数组无法用 FK 保证商品存在性和下架状态一致性。
3. 按作者/商品统计时，JSON 查询和索引都很别扭。
4. 商品下架、视频删除、作者解绑时，关联表的生命周期更清晰。

所以本设计**不改 `videos` 表结构**，只新增 `video_products` 关联表。

#### 新增迁移文件建议

```text
migrations/000007_create_commerce_core.up.sql
  products, skus, inventory, inventory_transactions

migrations/000008_create_video_products.up.sql
  video_products

migrations/000009_create_orders_payments.up.sql
  orders, order_items, payments

migrations/000010_create_author_sales_stats.up.sql
  author_sales_stats
```

每个迁移都配套对应的 `.down.sql`。

#### 与现有表的关系

- `products.merchant_id` 引用 `users.id`，但 `users` 表不需要新增字段。
- `video_products.video_id` 引用 `videos.id`，`video_products.author_id` 引用 `users.id`。
- `orders.user_id` 引用 `users.id`。
- 不修改 `users`、`videos`、`follows`、`likes`、`play_events` 的现有结构。
- 如果未来要支持完整商家体系，再单独加 `merchants` / `shops` 表，当前用 `merchant_id` 字段预留。

## 4. API 设计

### 4.1 新增接口总览

| 方法 | 路径 | 说明 | 认证 |
|---|---|---|---|
| GET | `/api/v1/products` | 商品列表 | 可选 |
| GET | `/api/v1/products/{id}` | 商品详情（含 SKU） | 可选 |
| POST | `/api/v1/videos/{id}/products` | 给视频挂商品 | 作者 |
| DELETE | `/api/v1/videos/{id}/products/{productId}` | 取消视频挂商品 | 作者 |
| GET | `/api/v1/videos/{id}/products` | 视频关联商品列表 | 可选 |
| POST | `/api/v1/orders` | 创建订单 | 用户 |
| GET | `/api/v1/orders/{id}` | 订单详情 | 用户 |
| GET | `/api/v1/orders` | 我的订单列表 | 用户 |
| POST | `/api/v1/orders/{id}/pay` | 模拟支付 | 用户 |
| POST | `/api/v1/orders/{id}/cancel` | 取消订单 | 用户 |
| GET | `/api/v1/authors/{id}/sales` | 作者带货统计 | 用户/作者 |
| GET | `/api/v1/authors/{id}/products` | 作者橱窗/带货商品列表 | 可选 |

### 4.2 创建订单请求示例

```http
POST /api/v1/orders
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "idempotency_key": "uuid-v7-or-client-generated-key",
  "items": [
    {
      "sku_id": 1,
      "quantity": 1
    }
  ],
  "referrer": {
    "author_id": 10,
    "video_id": 20,
    "product_id": 30
  }
}
```

### 4.3 创建订单响应示例

```http
HTTP/1.1 201 Created

{
  "order_id": 1001,
  "order_no": "20260814001",
  "status": "PENDING_PAYMENT",
  "total_amount": 9900,
  "expired_at": "2026-08-14T10:30:00Z"
}
```

### 4.4 模拟支付请求示例

```http
POST /api/v1/orders/{id}/pay
Authorization: Bearer <access_token>
Content-Type: application/json

{
  "payment_no": "pay-20260814-0001"
}
```

支付接口要求：

- `payment_no` 由调用方生成，服务端按 `payment_no` 幂等。
- 重复支付同一订单返回当前订单状态，不重复扣款。
- 只允许 `PENDING_PAYMENT -> PAID`。

### 4.5 错误码扩展

在现有错误码基础上新增：

| code | HTTP | 说明 |
|---|---|---|
| `PRODUCT_NOT_FOUND` | 404 | 商品不存在或已删除 |
| `SKU_NOT_FOUND` | 404 | SKU 不存在 |
| `PRODUCT_OFF_SALE` | 409 | 商品已下架，不可购买 |
| `INSUFFICIENT_STOCK` | 409 | 库存不足 |
| `ORDER_NOT_FOUND` | 404 | 订单不存在或无权访问 |
| `INVALID_ORDER_STATUS` | 409 | 非法订单状态转换 |
| `DUPLICATE_ORDER` | 409 | 重复下单（幂等冲突） |
| `PAYMENT_ALREADY_PAID` | 409 | 订单已支付 |
| `VIDEO_PRODUCT_EXISTS` | 409 | 视频已挂该商品 |
| `AUTHOR_SALES_NOT_FOUND` | 404 | 作者暂无带货数据 |

## 5. 关键流程设计

### 5.1 视频挂商品

```text
作者请求 POST /videos/{id}/products
  -> 校验视频存在且作者是视频作者
  -> 校验商品存在且 ON_SALE
  -> 插入 video_products
  -> 返回关联关系
```

要点：

- 作者只能给自己的视频挂商品。
- 商品下架不影响已挂载关系，但商品卡展示为不可购买。
- 删除视频时级联或软删除 `video_products`。

### 5.2 创建订单

```text
用户请求 POST /orders
  -> 根据 idempotency_key 查重
  -> 读取 SKU / 商品最新价格和状态
  -> 服务端计算总价
  -> 开启数据库事务：
      1. 扣减库存（条件更新，quantity >= 购买数量）
      2. 写 inventory_transactions
      3. 创建 orders（保存归因快照）
      4. 创建 order_items
  -> 提交事务
  -> 返回 PENDING_PAYMENT 订单
```

事务边界：

- 库存扣减和订单创建必须在同一个数据库事务内。
- 如果库存不足，回滚并返回 `INSUFFICIENT_STOCK`。
- 如果 `idempotency_key` 已存在，直接返回已有订单，不重复扣库存。
- **事务必须保持短小**：不要在事务内调用外部支付、发送通知、写缓存、调用风控。
  这些动作放在事务提交之后异步执行，避免长时间占用数据库连接和行锁。

### 5.3 模拟支付

```text
用户请求 POST /orders/{id}/pay
  -> 校验订单归属
  -> 校验订单状态是 PENDING_PAYMENT
  -> 按 payment_no 幂等
  -> 更新 payments 为 SUCCESS
  -> 更新 orders 为 PAID，记录 paid_at
  -> 返回订单状态
```

重复支付：

- 同一个 `payment_no` 重复请求直接返回成功结果。
- 不同 `payment_no` 对已支付订单返回 `PAYMENT_ALREADY_PAID`。

### 5.4 订单超时关闭

方案：定时任务扫描 + 补偿。

```text
定时任务每分钟执行：
  SELECT id FROM orders
  WHERE status = 'PENDING_PAYMENT'
    AND expired_at < now()
  LIMIT 500

对每个订单：
  开启事务：
    条件更新 status = 'EXPIRED' WHERE id = ? AND status = 'PENDING_PAYMENT'
    如果更新成功：
      回补库存
      写 inventory_transactions（reason = ORDER_EXPIRED）
```

要求：

- 使用条件更新防止并发关闭。
- 回补库存必须幂等，通过 `inventory_transactions.biz_id` 去重。
- 如果定时任务失败，下一次扫描继续处理。

### 5.5 作者带货统计

统计口径：

- 只统计 `PAID` 及之后的订单。
- 按 `orders.referrer_author_id` 分组。
- `order_count`：订单数。
- `buyer_count`：去重 `user_id` 数量。
- `gmv`：`SUM(total_amount)`。

实现方式：

- 初期直接 SQL 聚合，数据量小。
- 如果后续数据量大，再通过 `OrderPaid` 事件异步更新 `author_sales_stats`。
- 必须提供对账任务：用订单表原始数据重新聚合，与统计表比对。

### 5.6 库存扣减：仅用事务是否够？长期如何演进？

#### V1 结论：事务 + 条件更新够用

Commerce v1 是普通交易，不是秒杀，采用“数据库事务 + 条件更新”是正确的最小方案：

- 扣库存和创建订单在同一个短事务内，天然原子。
- 条件更新 `WHERE quantity >= ?` 防止超卖。
- `inventory_transactions` 提供流水，便于对账和排查。
- 幂等键保证重复请求不会重复扣减。

这个方案在单库、普通商品、低中并发下是**强一致、简单、可维护**的。

#### 但要注意“仅事务”的边界

仅靠数据库事务无法解决所有长期问题，必须提前认识到：

| 问题 | 事务方案的局限 | 长期演进方向 |
|---|---|---|
| 热点 SKU 行锁竞争 | 所有请求更新同一行库存，行锁串行化，吞吐有上限 | Redis 预扣 / 分片库存 / 异步队列削峰 |
| 事务内不能做外部调用 | 如果下单流程里加风控、通知、优惠券，会拉长事务 | 事务只做库存+订单；外部动作放到事务后异步执行 |
| 拆服务后无法跨库事务 | 订单服务和库存服务分开后，单库事务失效 | 领域事件 + Outbox / 本地消息表 + 补偿 |
| 秒杀流量 | 数据库行锁会成为瓶颈 | Redis Lua 预扣 + Kafka/Worker 异步落单 |
| 库存预扣后不支付 | 事务本身不会自动释放库存 | 超时关单 + 回补任务，已有设计 |
| 对账困难 | 只有库存数字，没有流水很难排查 | 必须保留 `inventory_transactions` |

#### 设计上从现在就要留好的演进点

1. **库存操作抽象成接口**，V1 用 DB 实现，未来可替换为 Redis + DB 实现。
2. **库存流水从第一天就记录**，不能只维护一个 `quantity` 数字。
3. **事务保持短小**，不要在事务里发通知、调外部支付、写缓存。
4. **订单状态机 + 补偿任务**是必须的，因为未来异步化后更需要补偿。
5. **对账任务**应该作为 Commerce v1 的一部分，而不是以后补。

#### 未来演进路线

```text
V1：数据库事务 + 条件更新 + 库存流水
  ↓ 出现热点商品/秒杀需求
V2：Redis Lua 预扣 + 数据库最终确认 + 异步队列
  ↓ 拆服务/独立库存中心
V3：库存独立服务 + 领域事件 + Outbox + 对账/补偿
```

所以“仅用事务保证扣减”在当前阶段可以接受，但不能把它当成终极方案。  
技术设计上要保证未来可以平滑替换，而不是推倒重来。

## 6. 幂等与并发设计

### 6.1 幂等

| 场景 | 幂等键 | 唯一约束 |
|---|---|---|
| 创建订单 | 客户端 `idempotency_key` | `(user_id, idempotency_key)` |
| 库存流水 | `order_id + reason` | `inventory_transactions.biz_id` |
| 模拟支付 | `payment_no` | `payments.payment_no` |
| 视频挂商品 | 无，业务上重复挂同一商品应报错或返回已有关系 | `(video_id, product_id)` |

### 6.2 并发控制

- 扣库存使用条件更新：

```sql
UPDATE inventory
SET quantity = quantity - $1, version = version + 1, updated_at = now()
WHERE sku_id = $2 AND quantity >= $1;
```

- 订单状态变更使用条件更新：

```sql
UPDATE orders
SET status = $new_status
WHERE id = $order_id AND status = $expected_status;
```

- 超时关闭和用户取消同时发生时，只有一个能成功。

## 7. 缓存设计

### 7.1 商品缓存

- 商品详情适合 cache-aside。
- key 示例：`product:detail:{id}`，TTL 加随机抖动。
- 商品下架/编辑时主动失效。
- 不缓存库存数量到 Feed，避免不一致。

### 7.2 Feed 商品卡查询设计

#### 不会 N+1：按页批量查询

Feed 本身已经是一次分页 SQL 返回当前页视频，不需要对每个视频单独查商品。  
正确做法是“按页批量组装”：

```text
1. Feed 分页查询返回当前页 videos（含视频字段）
2. 收集本页 video_ids
3. 一条 SQL 查 video_products
   WHERE video_id IN (...) AND status = 'ACTIVE'
4. 收集 product_ids
5. 一条 SQL / 缓存批量查 products / skus 摘要
6. 内存中按 video_id 分组，组装商品卡
```

假设每页 20 条视频，不是“20 次视频查询 + 20 次商品查询”，而是：

```text
1 次 Feed 分页查询
+ 1 次 video_products 批量查询
+ 1 次 products 批量查询
+ 1 次 skus 最低价批量查询（可选）
```

也就是每页固定增加 **2~3 次批量查询**，和页大小无关，不会出现 N+1。

#### 批量查询示例

```sql
-- 查视频关联商品
SELECT video_id, product_id, sort_order
FROM video_products
WHERE video_id IN (...)
  AND status = 'ACTIVE'
ORDER BY video_id, sort_order;

-- 查商品摘要
SELECT id, title, cover_key, status
FROM products
WHERE id IN (...);

-- 查 SKU 最低价（商品卡展示“起售价”）
SELECT product_id, MIN(price) AS min_price
FROM skus
WHERE product_id IN (...)
  AND status = 'ACTIVE'
GROUP BY product_id;
```

#### 缓存策略

- 视频关联关系缓存：`video_products:{video_id}`，短 TTL。
- 商品卡缓存：`product_card:{product_id}`，短 TTL，商品下架/改价时主动失效。
- 热门 Feed 第一页可以整页缓存商品卡，但要注意发布/挂商品/下架时的失效。
- 库存数量不进入 Feed 商品卡缓存，避免把交易数据暴露到内容缓存。

### 7.3 作者带货统计缓存

- 作者带货数据可以短 TTL 缓存，例如 30s。
- 写操作后不主动失效，接受短暂延迟。
- 对账任务保证最终一致。

## 8. 安全与校验

- 创建订单时金额以服务端计算为准，不信任客户端价格。
- 商品状态和 SKU 状态以下单时数据库为准。
- 订单查询必须校验归属：普通用户只能看自己的订单。
- 视频挂商品必须校验作者身份。
- 作者带货统计接口只暴露聚合数据，不暴露买家明细。
- 所有新增接口继续走 JWT 鉴权和限流。

## 9. 测试计划

### 9.1 单元测试

- 订单状态机合法/非法转换
- 幂等键查重
- 金额计算
- 库存扣减条件更新
- 归因快照保存
- 作者统计 SQL 聚合

### 9.2 集成测试

- 真实 PostgreSQL 事务：创建订单 + 扣库存原子性
- 重复创建订单不重复扣库存
- 并发扣库存不超卖
- 支付重复回调幂等
- 超时关闭与库存回补
- 视频挂商品权限校验
- 新迁移在已有 001~006 数据上可安全执行，不破坏现有 Feed/用户数据

### 9.3 契约测试

- 新增接口全部写入 `internal/api/openapi.yaml`
- 保持 route-sync 测试通过
- 前端类型通过 `openapi-typescript` 重新生成

### 9.4 压测（后续）

- 普通下单 QPS / P95 / P99
- 库存热点并发扣减
- 定时关单任务对数据库的压力

## 10. 实施顺序建议

1. 商品与 SKU：migration + CRUD + 商品详情
2. 库存与库存流水：扣减、回补、对账
3. 视频挂商品：`video_products` + 接口
4. 普通下单：幂等、事务、订单状态机
5. 模拟支付：支付幂等 + 状态流转
6. 超时关单：定时任务 + 库存回补
7. 作者带货统计：SQL 聚合 + 接口
8. Feed 商品卡：批量商品查询 + 前端展示
9. OpenAPI / 测试 / 文档收尾

## 11. 非目标与未来演进

- 秒杀：在普通订单稳定后，再引入活动库存、Redis 预扣、队列削峰。
- Kafka：当前无压测数据证明需要，不在 Commerce v1 引入。
- 微服务：继续模块化单体，直到出现独立扩缩容或团队边界需求。
- 多商家：先以 `merchant_id` 字段预留，不做完整店铺体系。
- 库存扣减：V1 先用数据库事务 + 条件更新；只有出现热点商品/秒杀等可测量瓶颈后，再演进到 Redis 预扣 + 异步确认。

## 12. 完成定义

Commerce v1 完成时：

- [ ] 商品、SKU、库存可管理
- [ ] 作者能给自己的视频挂商品
- [ ] Feed / 视频详情能展示商品卡
- [ ] 用户能下单、支付、查看订单
- [ ] 重复请求不产生重复订单
- [ ] 并发下单不超卖
- [ ] 超时未支付订单自动关闭并回补库存
- [ ] 系统能按作者统计订单数、购买人数、GMV
- [ ] OpenAPI 契约和 route-sync 通过
- [ ] 后端测试全绿

## 13. 尚未覆盖 / 需要进一步确认的点

以下内容在当前技术设计中还没有完全展开，进入开发前需要补充决策。

### 13.1 权限与角色

- 谁可以创建商品 / 上下架商品？
  - 建议：`MERCHANT` / `ADMIN` 角色。
- 作者挂商品时，是否只允许挂 `ON_SALE` 商品？
- 作者带货统计接口是否只允许作者本人 / 管理员查看？
- `products.merchant_id` 与 `users.role` 的关系需要应用层强制，不能只靠字段。

### 13.2 归因可信度

- 创建订单时客户端传的 `referrer` 不能直接信任。
- 需要校验 `video_products` 中是否存在 `(video_id, author_id, product_id)` 且状态为 `ACTIVE`。
- 需要决定：校验失败是拒绝下单，还是忽略归因继续下单？
  - 建议：校验失败拒绝下单，避免伪造作者数据。

### 13.3 订单取消与支付超时

- 用户取消订单的流程还没有展开：
  - 只能取消 `PENDING_PAYMENT`
  - 取消后需要回补库存
  - 写 `inventory_transactions`，reason = `ORDER_CANCELED`
- 支付超时后如果支付请求才到达：
  - 是拒绝支付，还是允许支付？
  - 建议：拒绝，订单已 `EXPIRED`，走退款/补偿（当前无真实支付，直接拒绝即可）。
- 用户取消和超时关闭并发时，依赖状态条件更新保证只有一个成功。

### 13.4 订单号与幂等键

- `order_no` 的生成规则还没定：
  - 时间戳 + 随机数？
  - 数据库序列？
  - 雪花 ID？
- `idempotency_key` 需要保留多久？
  - 建议至少保留到订单生命周期结束，并定期清理过期数据。
- 同一个 `idempotency_key` 携带不同请求内容时，返回已有订单还是冲突？
  - 建议：返回已有订单，或明确返回 `DUPLICATE_ORDER`。

### 13.5 定时任务运行位置

- 订单超时关闭任务跑在哪里？
  - 当前项目没有 `cmd/worker`。
  - 选项 1：API 进程内启动一个后台 ticker。
  - 选项 2：新增 `cmd/worker`，独立进程。
  - 需要根据部署方式决定。
- 定时任务要支持多实例安全：同一订单不能被多个实例同时关闭。
  - 通过条件更新 `WHERE status = 'PENDING_PAYMENT'` 保证。

### 13.6 前端与契约

- 需要补充前端页面：
  - 商品详情页
  - 订单列表 / 订单详情
  - 作者带货中心
  - 视频发布时选择商品
- 所有新接口需要同步更新 `internal/api/openapi.yaml`。
- 前端类型通过 `openapi-typescript` 重新生成。

### 13.7 配置与可观测性

- 新增配置项：
  - `ORDER_TTL`：订单超时时间
  - `PRODUCT_CACHE_TTL`：商品缓存时间
  - `COMMERCE_ENABLED`：功能开关
- 新增指标：
  - 订单创建 QPS / 成功率
  - 库存扣减失败次数
  - 超时关单数量
  - 作者统计对账差异
- 日志需要包含：`user_id`、`order_id`、`sku_id`、`product_id`、`author_id`。

### 13.8 数据生命周期

- 商品删除建议使用软删除，避免破坏历史订单和 `video_products` 引用。
- SKU 下架后，已创建的订单仍使用 `order_items` 快照，不受影响。
- `video_products` 在视频删除时如何处理？
  - 级联删除？
  - 软删除？
  - 需要与视频模块现有删除语义对齐。

### 13.9 并发与数据库

- 多 SKU 下单时，建议按 `sku_id` 排序后扣库存，减少死锁概率。
- 如果出现数据库死锁或唯一冲突，是否需要重试？
  - 建议对可重试错误做有限重试。
- 事务隔离级别是否需要调整？
  - 默认 Read Committed 通常够用，压测后再评估。

### 13.10 统计口径

- `GMV` 是指订单金额还是实付金额？
  - 当前没有优惠券/退款，建议先定义为“已支付订单的总金额”。
- `buyer_count` 的去重窗口是什么？
  - 建议按作者维度对 `user_id` 去重。
- 如果后续加入退款，统计表需要支持回退或重算。

### 13.11 安全与防刷

- 创建订单接口需要防刷：
  - 用户级限流已有，但可能需要按 `idempotency_key` 做短期去重。
- 模拟支付接口不能成为“免费下单”漏洞：
  - 只能由订单本人调用。
  - 建议只允许 `PENDING_PAYMENT` 订单支付。
- 商品挂载接口需要防止作者给别人的视频挂商品。

### 13.12 性能边界

- Feed 批量查询的 `IN (...)` 数量需要设上限，建议不超过页大小。
- 商品卡缓存失效要考虑“商品下架后 Feed 仍展示旧卡片”的短暂不一致。
- 如果 Feed 页大小变大，需要重新评估批量查询和缓存策略。

## 14. 开发准入检查

### 14.1 当前是否可以开始开发？

结论：**可以开始第一阶段开发，但不要一次性全量铺开。**

建议按以下阶段推进，每个阶段有明确的“开始条件”和“完成条件”。

### 14.2 第一阶段：商品 / SKU / 库存（可以立即开始）

这部分不依赖大部分未决事项，适合先做：

- 新增迁移：`products`、`skus`、`inventory`、`inventory_transactions`
- 商品 CRUD
- SKU CRUD
- 库存查询 / 库存调整
- 商品上下架
- 基础测试

开始条件：

- 确定谁可以管理商品，建议先默认 `MERCHANT` / `ADMIN`
- 商品删除使用软删除

完成条件：

- 商品、SKU、库存接口可用
- 迁移可在现有数据库上安全执行
- 后端测试通过

### 14.3 第二阶段：视频挂商品（可以开始）

依赖第一阶段，但还不依赖订单：

- 新增 `video_products` 迁移
- 作者给自己的视频挂商品 / 取消挂商品
- 查询视频关联商品

开始条件：

- 确认作者只能操作自己的视频
- 确认商品下架后关联关系保留，但商品卡不可购买

完成条件：

- 作者能挂商品
- Feed / 视频详情能查询到关联商品 ID

### 14.4 第三阶段：下单 / 支付 / 超时关单（进入前必须先决策）

进入本阶段前，以下问题必须冻结：

1. 创建订单时 `referrer` 是否必须通过 `video_products` 校验？
2. 校验失败是拒绝下单，还是忽略归因？
3. 用户取消订单和超时关单的库存回补流程
4. 支付超时后，迟到的支付请求如何处理？
5. 超时关单任务跑在 API 进程内，还是独立 `cmd/worker`？
6. `order_no` 生成规则
7. `idempotency_key` 重复且内容不一致时，返回已有订单还是冲突？
8. 商品/视频删除时，关联数据和历史订单如何处理？

这些不解决，下单相关代码容易返工。

### 14.5 第四阶段：作者带货统计 / Feed 商品卡 / 前端

进入前需要确认：

- `GMV` 口径
- `buyer_count` 去重口径
- 作者统计接口的可见权限
- 商品卡 DTO 字段
- 是否第一版就做前端页面

### 14.6 建议

```text
现在就可以开始：
  商品 / SKU / 库存 / 迁移

下一阶段前需要决策：
  归因校验、订单取消/超时、任务运行位置、统计口径

更后期再做：
  Feed 商品卡前端、秒杀、Kafka、微服务
```
