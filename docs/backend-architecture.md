# 后端架构与模块边界

> 更新于 2026-09-26。以 [`cmd/api/main.go`](../cmd/api/main.go)、[`OpenAPI`](../internal/api/openapi.yaml) 和 [`migrations`](../migrations/) 为当前事实来源。TokMall 是模块化单体，Hertz API 在一个进程中装配各业务模块，PostgreSQL 是交易事实库。

## 分层

```text
HTTP 请求
  → Hertz 路由 / requestid / recovery / auth / 限流
  → 模块 Handler（HTTP DTO、错误码映射）
  → 模块 Service（业务规则、消费侧接口）
  → Repository（GORM/SQL、事务、领域错误翻译）
  → PostgreSQL / Redis / MinIO
```

`cmd/api/main.go` 是唯一装配点；`internal/api/openapi.yaml` 是公开接口契约，`cmd/api/main_test.go` 双向检查路由与契约。数据库仅通过版本化 SQL 迁移修改，禁止 AutoMigrate。`/metrics` 在独立 9091 端口暴露。

## 业务模块

| 模块 | 主要职责 | 拥有的事实 |
|---|---|---|
| `user` | 注册/登录、刷新旋转/登出、主页 | `users` |
| `video` | 上传票证、发布/删除、播放事件、详情与作者视频列表 | `videos`、`upload_tickets`、`play_events` |
| `interaction` | 点赞/关注幂等、社交列表 | `likes`、`follows` |
| `feed` | 时间游标、关注 Feed、首页缓存、商品卡装配 | 不拥有视频或交易事实 |
| `product` | 商品/SKU、状态与查询 | `products`、`skus` |
| `inventory` | 商家库存调整和流水 | `inventory`、`inventory_transactions`；订单事务会在同库扣减/回补 |
| `order` | 单 SKU 订单、模拟支付、取消/过期、查询和履约状态 | `orders`、`order_items`、`payments` |
| `promotion` | 视频与商品挂载、排序和取消 | `video_products` |
| `attribution` | 作者成交统计 | 无写入；读取已支付订单和订单项 |

业务模块不共享 Go model 类型。跨模块只读表时，在消费模块声明自己的 read model 或直接写 SQL；写入规则尽量由拥有事实的模块承担。订单与库存目前共享一个 PostgreSQL 事务，这是模块化单体的显式一致性边界，不应伪装成跨服务事务。

## 主要装配关系

| 消费侧需要 | 由 `main.go` 注入的实现 | 用途 |
|---|---|---|
| `user.ImageUploadValidator`、`product.ImageTicketValidator` | `video.Service` | 头像/商品封面上传票证校验 |
| `video.FeedCacheInvalidator` | `feed.RedisFeedCache` | 视频状态变化后失效 Feed 缓存 |
| `interaction` 的关注 Feed 失效接口 | 关注 Feed Redis 缓存实现 | 关注关系变化后失效对应首页 |
| `feed`/`video` 的商品卡读接口 | 各自的 PostgreSQL read model | 批量装配商品卡，避免 N+1 |
| `feed` 的播放 URL、缓存和统计接口 | `storage.Client`、Redis 缓存、Prometheus collector | 短效预签名 URL 与缓存观测 |
| 各模块的 Repository 接口 | 各自 `GormRepository` | 保持 Service 不依赖 GORM 类型 |

`order.ExpiryWorker` 由 API 进程按配置启动，扫描待支付到期订单，用 `FOR UPDATE SKIP LOCKED` 支持多实例竞争；它不是 Kafka consumer，也没有独立 Worker 进程。

## 数据流中的三个关键边界

1. **内容与交易**：Feed/视频详情展示商品卡，但不修改库存和订单。下单时订单模块重查 SKU/商品状态、服务端单价与挂载来源，并在事务里保存快照。
2. **作者与商家**：`video_products.author_id` 是推广作者，`products.merchant_id` 是商品所有者。当前推广授权简化为只能挂本人商品；订单同时保留商家与来源作者，成交统计按来源作者聚合。
3. **缓存与事实**：Feed 主体可缓存，商品卡在响应装配时按页动态读取，因此下架状态不会被旧 Feed 缓存固定住。Redis 缓存失效或限流故障不能改变 PostgreSQL 交易事实。

订单状态、幂等和数据库不变量见 [Commerce v1 当前技术实现](commerce-v1-technical-design.md)；历史 Feed 方案见 [Feed Mentor 手册](feed-system-mentor-guide.md)，其早期目录与“下一步”描述不作为当前架构依据。
