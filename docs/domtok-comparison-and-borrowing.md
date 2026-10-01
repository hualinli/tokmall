# DomTok 对比与借鉴报告

> 对比对象：[west2-online/DomTok](https://github.com/west2-online/DomTok)（2025 字节跳动青训营第一名，
> Kitex + Hertz + MySQL + Redis + RocketMQ + Kafka + Elasticsearch + Eino 的分布式电商后端）。
> 阅读方式：完整 clone 后逐模块精读（`app/order`、`app/payment`、`app/commodity`、`app/cart`、`app/user`、
> `pkg/*`、`config/*`、`docker/*`、`hack/*`、`.github/workflows/*`、`idl/*`、`config/sql/*`），
> 行号对应该仓库 checkout `e434d96`。
>
> 本文回答一个问题：**DomTok 里有什么值得搬到 TokMall 当亮点，有什么必须绕开。**
> 结论按"借鉴价值 × 落地成本"排序，并在 §4 给出本轮真正落地的四项（含代码位置与验证证据）。

---

## 1. 两个项目的关系与差异

| 维度 | DomTok | TokMall |
|---|---|---|
| 形态 | 7 个 Kitex 微服务 + Hertz 网关 + Thrift IDL | 模块化单体（Hertz 单进程，模块边界按未来拆分设计） |
| 数据 | MySQL（8 张商域表）+ Redis + ES | PostgreSQL（18 个迁移）+ Redis + MinIO |
| 一致性 | 分布式锁（redsync）+ 延迟消息 + Redis 缓存判据 | 条件更新 + DB CHECK + 事务 + Transactional Outbox + 对账 |
| 消息 | RocketMQ（延迟消息）+ Kafka（ES 同步/购物车写入） | Outbox + Redis Streams（至少一次 + 幂等消费 + DLQ + 重放） |
| 观测 | OTel Collector + Jaeger + Prometheus + VictoriaMetrics + EFK + Grafana（17 个容器） | 应用级 Prometheus 指标 + slog + 对账 + 演练脚本 |
| 业务 | 购物车、优惠券、SPU/SKU、库存预扣、支付、退款、AI 助手（Eino Function Calling） | 内容 Feed、商品/SKU/库存、订单/支付/履约、售后退款、归因、对账 |
| 工程 | golangci-lint 27 个 linter、CodeQL、govulncheck、license 检查、hack 脚本 | CI 四 job、契约 schema 级校验、两个演练脚本、压测客户端 |

一句话概括这次对比的结论：

> **DomTok 的"架构外壳"（分层、IDL、观测栈、工程流水线）值得学；
> 它的"交易内核"（库存、金额、状态机、缓存判据、消息可靠性）在 TokMall 现有实现面前是反面教材。**

这不是贬低：DomTok 是一个 7 人学生团队在青训营周期内交付的微服务作品，它的价值在于
"把分布式电商的全套组件跑通了"；而 TokMall 的目标是"把少数几件事做到可证明"。
两者取舍不同，正好互补——**这也是本次借鉴能成立的前提**。

---

## 2. 必须绕开的坑（带证据）

这一节是本次对比中最有价值的部分：这些坑**看起来都很专业**，照抄会直接把已经修好的问题重新引入。

### 2.1 交易内核

| # | 坑 | 证据（DomTok） | 为什么不能抄 |
|---|---|---|---|
| 1 | **"预扣库存"根本不校验库存** | `app/commodity/infrastructure/mysql/db.go:485` 只判 `lockStock < 0`；真正的校验在支付后的 `:432-437` | 预扣没有预留语义：100 人抢最后 1 件会全部预扣成功，支付后才发现超卖，而钱已经记账。TokMall 的 `UPDATE ... WHERE quantity >= n` 直接胜出 |
| 2 | **订单/支付状态无条件更新** | `app/order/infrastructure/mysql/db.go:213-224` 无 `WHERE status = from` | 并发支付会重复扣库存；TokMall 靠条件更新 + `RowsAffected` 判幂等 |
| 3 | **支付状态码写进订单状态列** | 同上的 `Updates{"status":…, "payment_status":…}`；两套枚举在 1/2 上撞车 | 后果是"已支付订单在业务上等于已完成"（`usecase/service.go:103,129` 据此拒绝取消/改地址）。TokMall 用独立状态 + CHECK 约束避免了它 |
| 4 | **把支付状态放进 Redis 当判据** | `app/order/domain/service/mq.go:52-59` 缓存优先；写路径两处 bug（`service.go:136` 传空结构体、`mq.go:69` 未设 OrderID） | 已支付订单的缓存永远是 pending，10 分钟后的延迟消息据此**对已支付订单发起库存回滚**。教训：**状态类数据以 DB 为唯一真相，缓存只能加速展示** |
| 5 | **金额 float64 贯穿全链路** | DDL `DECIMAL` → GORM/领域模型 `float64` → thrift `double`；`cart/infrastructure/rpc/rpc.go:77` 用 `int64(price)*count` 算总价 | 9.99 元按 9 元计；字段语义错位导致订单应付金额恒 0（"0 元购"）。TokMall 从第一天就用整数分 |
| 6 | **Redis 当库存事实源 + 双写非原子** | `app/commodity/domain/service/service.go:592-612` errgroup 并发写 Redis 与 DB；key TTL 24h，冷 key 直接报错（`:475-479`） | Redis 成功 DB 失败就永久不一致；24h 无流量后下一个下单直接失败。TokMall 的 ADR-019 明确拒绝在没有可测瓶颈时引入 Redis 预扣 |
| 7 | **自研 Redis 锁** | `app/commodity/infrastructure/redis/lock.go:39-79`：`SETNX(key,0,3s)` + 裸 `DEL`；多 key 无失败回滚 | 无 owner token（任何人可解别人的锁）、TTL 短于业务、多 SKU 订单会自死锁。TokMall 用数据库行锁，不需要分布式锁 |
| 8 | **MQ 当唯一写路径** | `app/cart/usecase/add_goods.go:36` 只发 Kafka 就返回 200；消费失败只打日志（`consume_add_goods.go:36-43`） | 用户以为加购成功其实丢了；重复投递会重复累加。正解是 TokMall 的 Outbox（同事务 + 至少一次 + 幂等消费） |
| 9 | **退款不写金额、不处理优惠、不与账目联动** | `payment.sql:24` 的 `refund_amount NOT NULL` vs Go 模型缺该字段 | 退款无法对账。TokMall 的退款单带金额且被六项对账检查覆盖 |
| 10 | **地址簿 IDOR + 归属字段恒 0** | `app/user/infrastructure/mysql/db.go:105-137` 只按主键查地址、创建时不写 `user_id` | 水平越权（P0）。TokMall 的归属校验统一在 service 层做，越权一律 404 |
| 11 | **JWT 每请求重签 + 丢弃 token 类型** | `app/gateway/mw/auth.go:33-53`（`CreateAllToken`）、`:34`（`_, uid, err :=`） | refresh token 可当 access 用；滑动续期没有轮换/重放检测。TokMall 的 refresh 旋转 + 黑名单（ADR-011）更强 |
| 12 | **购物车 JSON 读改写 + shadow 变量 bug** | `app/cart/domain/model/cart_json.go:135-150`（已复现：买走 SKU 后不清车，还会复制末位 SKU） | 文档型存储的并发丢更新 + Go slice 值语义，两个坑叠在一起 |

### 2.2 观测与工程（同样"看起来专业"）

| # | 坑 | 证据 | 说明 |
|---|---|---|---|
| 13 | **应用自己不暴露指标** | 全仓库 `app/ pkg/ cmd/` 下 grep `prometheus\|promhttp` 零命中；Grafana 的"Go 面板"实际查的是 cadvisor 容器指标 | 17 个观测容器 ≠ 应用可观测。TokMall 的 db_pool/cache/outbox/reconcile 指标更贴近业务 |
| 14 | **HTTP 一律 200，错误码塞 body** | `app/gateway/pack/base.go:39-45` | 网关/监控/客户端重试策略全部失效。TokMall 有显式状态码映射表 |
| 15 | **内部错误文本透传客户端** | `pkg/errno/errno.go:103-105` 把 `err.Error()` 放进返回给调用方的 `ErrorMsg` | 信息泄漏。TokMall 未知错误一律 `INTERNAL` |
| 16 | **trace 与 log 互不关联** | 日志无 trace_id 字段；`pkg/logger/klog.go:100-102` 的 `Ctx*` 方法直接丢弃 ctx；网关也没有 HTTP server span | "有链路无关联"：Jaeger 里能看到拓扑，却无法从日志跳到那条 trace |
| 17 | **CI 关掉模块缓存、lint 用 latest、vulncheck 监听不存在的分支、死代码步骤** | `ci.yaml:25,44,59,63,79-87`、`vulncheck.yml:4-5` | 不可复现 + 无效流水线。TokMall 反向照做（见 §4.4） |
| 18 | **Kafka 消费 log-and-drop** | `app/cart/domain/service/consume_add_goods.go:36-43` 反序列化失败后继续用零值对象 | 用 `uid=0` 写购物车。TokMall 的消费者把失败留在 PEL，超限进 DLQ |

---

## 3. 值得借鉴的清单（按性价比排序）

成本口径：小 = 半天内、无新依赖；中 = 1~2 天或有新依赖；大 = 需要新基础设施或跨模块改造。

| # | 借鉴点 | DomTok 怎么做 | 面试价值 | 成本 | 本轮是否落地 |
|---|---|---|---|---|---|
| 1 | **布隆过滤器 + 空值缓存防缓存穿透** | `app/gateway/service/bloomfilter.go` 用多片布隆过滤器做用户维度前置判断 | ⭐⭐⭐⭐⭐ "缓存三兄弟（穿透/击穿/雪崩）"一次讲全，且有压测数据 | 小 | ✅ §4.1 |
| 2 | **延迟消息做订单超时** | RocketMQ `DelayLevel=10min` + 消费前查支付状态（`create_order.go:131-134`、`mq.go:28-49`） | ⭐⭐⭐⭐⭐ 与 TokMall 的轮询 Worker 形成完整对照："MQ 只能当加速器，不能当唯一调度器" | 小 | ✅ §4.2 |
| 3 | **请求级可观测性：关联 ID 自动贯穿 + traceparent + 路由模板指标** | DomTok 反而没做（见坑 16） | ⭐⭐⭐⭐⭐ "一条日志串起一次请求" + 指标基数治理 | 小 | ✅ §4.3 |
| 4 | **CI/工程流水线抄作业** | `.golangci.yml` 27 个 linter、CodeQL、govulncheck、"自动修复后比 diff"、`go mod tidy` 漂移检查 | ⭐⭐⭐⭐ 展示"我知道哪些检查能防住哪类退化" | 小 | ✅ §4.4 |
| 5 | **GORM 日志策略：参数化 + 忽略 record not found + 慢查询阈值** | `pkg/base/client/db.go:52-55` | ⭐⭐⭐ 日志合规 + 噪声治理；压测里实测出 10 倍差距 | 小 | ✅ §4.1 附带 |
| 6 | **库存"可售量 = stock − locked"三段式语义** | `sku_info(stock, lock_stock)`：下单预占、支付实扣、超时释放 | ⭐⭐⭐⭐ 讲清"可售量 vs 实物库存"与超卖边界 | 中 | ⏳ 见 §5 |
| 7 | **SKU 价格版本表 + 全链路 version_id** | `sku_price_history` + 订单 `goods_version` | ⭐⭐⭐⭐ 价格审计、"下单时价格"的可证明性 | 中 | ⏳ 见 §5 |
| 8 | **优惠券/促销域** | `coupon_info` + `user_coupon` + 贪心选券 | ⭐⭐⭐⭐ 防超发、一券一单、退款退券、金额口径与对账 | 大 | ✅ §4.6（按它的反面清单重新设计） |
| 9 | **熔断/降级（依赖级）** | Sentinel 流控（`app/gateway/mw/sentinel.go`）+ Redis 健康探测双路径降级 | ⭐⭐⭐⭐ 限流 vs 熔断 vs 降级的区别，有真实代码 | 中 | ✅ §4.5 |
| 10 | **两级缓存（进程内 L1 + Redis L2）+ 失效广播** | DomTok 没有本地缓存 | ⭐⭐⭐⭐ 多级缓存一致性与 QPS 提升 | 中 | ⏳ 见 §5 |
| 11 | **错误在"创建点"抓栈 + 错误日志分流** | `pkg/errno` 的 `callers()`（延迟符号化）+ `pkg/middleware/log.go:40-63` | ⭐⭐⭐ "错误现场是构造它的那一行" | 小 | ⏳（TokMall 的哨兵错误是单例，需改造才适用） |
| 12 | **Prometheus recording rule 命名 `level:metric:ops` + 告警规则** | `config/prometheus/mysqld_rules.yml:4-8` | ⭐⭐⭐ 高基数留在预聚合里，看板只查低基数 | 小 | ⏳ 见 §5 |
| 13 | **支付一次性令牌（Lua GET+DEL，保留剩余 TTL 以便回滚）** | `app/payment/infrastructure/redis/script.go:47-57` | ⭐⭐⭐ 防重放 + 原子消费令牌 | 小 | ⏳ 见 §5 |
| 14 | **下单固化商品快照** | `order_goods` 冗余标题/规格/单价 | ⭐⭐⭐ "订单是历史事实" | 小 | 已有（TokMall order_items 快照） |
| 15 | **bigint 主键在 JSON/ES 里序列化为 string** | `model.SpuES` 用 string 存 spu_id | ⭐⭐ JS `MAX_SAFE_INTEGER` 精度坑 | 小 | 已有（TokMall 用 int64 + 前端 number 安全范围内） |
| 16 | **AI 助手（Eino Function Calling）** | `app/assistant` 把自然语言映射成接口调用 | ⭐⭐⭐⭐ 差异化亮点，但需要 LLM Key 与外部依赖 | 大 | ✖ 不建议（与"可验证"的项目基调冲突） |

---

## 4. 本轮落地的四项

四项都遵循同一条原则：**借思路，不借实现**；落地时必须比 DomTok 的原版更严格（有测试、有指标、有边界说明）。

### 4.1 缓存穿透防护（布隆过滤器 + 负缓存 + 重建）

**DomTok 的做法**：网关维护一个布隆过滤器切片（`app/gateway/service/bloomfilter.go`），
满一片换新片、超过 20 片丢最老的，用来判断"这个用户是否可能被封禁"。
问题：纯内存态、启动不预热（漏判会让封禁用户继续访问）、只服务于一个场景。

**TokMall 的做法**（`internal/existence/`）：

1. **自己实现布隆过滤器**（`guard.go`）：位数组长度与哈希个数按标准公式推导
   （`m = -n·ln p/(ln2)²`、`k = (m/n)·ln2`），用 splitmix64 双哈希；
   测试直接对照理论误判率（20,000 元素、目标 1% → **实测 1.012%**），并断言**假阴性恒为 0**。
2. **双缓冲轮换**（`Rotating`）：布隆过滤器不支持删除，业务却在删除；
   定期全量重建写入 standby 再切换，重建期间新旧两份都查 → 不产生假阴性。
3. **warmup 保护**：首次装载完成前 `Ready()=false`，此时**放行所有请求**——
   空过滤器会对一切 ID 返回"不存在"，那是灾难性假阴性。
4. **负缓存**（Redis，`redis.go`）：确认不存在的 ID 写入 `existence:missing:{ns}:{id}`，
   TTL 带**随机抖动**（顺带解决缓存雪崩的最小形态）。
5. **只对"记录不存在"写负缓存**：未发布视频对非作者也返回 404，但那是可见性拒绝，
   写进负缓存会让作者本人在 TTL 内看不到自己的视频（有专门的测试锁死这条）。
6. **指标**：`existence_lookup_total{namespace,result}` 直接回答"请求最终走到哪一层"。

**实测（本机，30 并发 × 10s，请求随机不存在的视频 ID）**：

| 配置 | 吞吐 | p50 | p99 | 数据库查询 |
|---|---|---|---|---|
| 关闭防护 | 2,996 req/s | 5.7ms | 60ms | 每次请求都查库 |
| 开启防护 | 5,086 req/s | 1.3ms | 50ms | **0**（全部被布隆过滤器挡下） |
| 关闭防护（修 GORM 日志前） | 281 req/s | 36.8ms | 7.8s | 每次请求都查库 + 每次写一条日志 |

第三行是意外收获：GORM 默认把每条 `record not found` 都当错误打日志，
穿透压测下日志 I/O 成了真瓶颈。顺手借鉴 DomTok 的 GORM 配置
（`ParameterizedQueries` + `IgnoreRecordNotFoundError` + `SlowThreshold`）后，
**同一场景吞吐提升 10 倍**。这也说明"日志策略"是性能与合规的双重开关。

### 4.2 延迟事件加速器 + 轮询安全网（订单超时）

**DomTok 的做法**：下单后发一条 RocketMQ 延迟 10 分钟的消息，消费端查
（缓存里的）支付状态决定是否回滚库存。**问题是把消息当成了事实源**：
broker 单主 ASYNC_FLUSH、消息无幂等键、DLQ 无人消费——丢一条消息 = 一笔订单的库存永久泄漏。

**TokMall 的做法**（`internal/order/timeout_handler.go` + `internal/outbox`）：

```text
下单事务 ──写 outbox（available_at = expires_at）──> 中继到点投递 ──> TimeoutHandler
    └── expires_at 同时落库，轮询 Worker 每 5s 扫一次作为安全网
```

- **延迟能力来自数据库字段**：`outbox_events.available_at` 本来用于重试退避，
  这里复用它实现延迟队列——不需要 RocketMQ、不需要 ZSET 调度器、不新增组件。
- **数据库是唯一事实源**：消费侧不信任事件内容，回查订单状态并用
  `WHERE id=? AND status='PENDING_PAYMENT' AND expires_at<=now()` 条件更新关单；
  已支付/已取消/已被 Worker 关掉的订单一律 0 行受影响 → no-op（幂等）。
- **消息全丢也能收敛**：Worker 仍在跑，最坏情况退化为原来的轮询语义。
- **可观测**：`order_timeout_event_total{result=expired|noop|failed}` ——
  noop 占比高说明 Worker 已经够快（消息只是锦上添花），expired 占多数说明加速器在承担主责。

**验证**（`internal/order/refund_postgres_integration_test.go::TestPostgresDelayedTimeoutEvent`）：

1. 事件与订单同事务写入，且 `available_at` 精确等于 `expires_at`（早投/晚投都会破坏语义）；
2. 未到期时中继 `FetchPending` **取不到**它（这就是"延迟"）；
3. 到期后中继取到 → handler 关单 → 订单 EXPIRED、库存回补；
4. 已支付订单收到超时事件 → no-op，状态与库存都不变。

### 4.3 请求级可观测性（关联 ID + traceparent + 路由模板指标）

DomTok 有 OTel/Jaeger 全家桶，但**trace 与 log 是两条互不相关的数据流**
（日志里没有 trace_id，网关连 HTTP server span 都没有）。TokMall 反过来做：
不引入 OTel SDK，先拿到 90% 的价值。

1. **`observability.ContextHandler`**（slog 装饰器）：从 context 自动注入
   `request_id / trace_id / span_id / route / user_id`，业务代码只要
   `slog.InfoContext(ctx, ...)` 就自动带上——**不依赖任何人自觉**；
   同时按字段名黑名单脱敏（`password/token/authorization/...` → `***`），组内字段也覆盖。
2. **`internal/tracing`**：自己实现 W3C `traceparent` 的解析/生成/透传（<100 行，零依赖）。
   **严格校验**：版本、长度、hex、非全 0，非法一律丢弃并重新生成——
   追踪头来自客户端，放行任意字符串等于允许往日志字段里注入内容。
3. **`middleware.Trace` + `middleware.AccessLog`**：每个请求一条结构化访问日志
   （method/route/status/duration_ms/bytes/client_ip），4xx 记 Warn、5xx 记 Error，
   业务失败不污染错误率。
4. **`observability.HTTPMetrics`**：`http_server_duration_seconds{method,route,status}` 直方图，
   **route 用路由模板而不是原始路径**——`/videos/1` 与 `/videos/2` 是同一个时间序列，
   否则路由参数一多就是基数爆炸。

升级路径也写清楚了：真拆服务时把 `internal/tracing` 换成 OTel SDK + OTLP exporter 即可，
上下文传播语义已经对齐（同为 W3C TraceContext）。

### 4.4 CI 与工程流水线

借鉴 DomTok 的 CI 设计（并修掉它自己的问题）：

| 借鉴 | DomTok | TokMall |
|---|---|---|
| `go mod tidy` 漂移检查 | ✅ | ✅ 新增（防"本地 tidy 过、CI 里 go.sum 不一致"） |
| "自动修复后比 diff" | ✅ fmt/vet/license | ✅ fmt + 契约生成物漂移检查 |
| CodeQL / govulncheck | ✅（但 vulncheck 监听不存在的分支） | ✅ 新增独立 workflow，监听 main + PR + 每周定时 |
| 最小权限 / 超时 / 并发取消 | ✖ | ✅ `permissions: contents: read`、`timeout-minutes`、`concurrency` |
| 文档改动跳过重活 | ✅ | ✅ `paths-ignore` |
| 一键复现 CI | ✅ `make verify` | ✅ `make verify` + `scripts/preflight.sh` |
| 模块缓存 | ✖ `cache: false` | ✅ 全部开启 |

### 4.5 依赖级熔断器（对象存储）

**DomTok 的做法**：Sentinel 做入口流控（全局 QPS=100，粒度过粗），另有一套
"Redis 健康探测 + 双路径切换"（`IsHealthy()` 每 5 秒 PING，健康走 Redis+DB 双写、
不健康走纯 DB）。**问题**：它把熔断和"选哪条实现路径"混在一起，
两套语义静默切换（同一业务在两套一致性模型之间来回跳）。

**TokMall 的做法**（`internal/breaker/`）：标准三态熔断器 + 装饰器，业务代码零改动。

- 状态机：closed →（连续失败达阈值）→ open →（冷却期，指数退避）→ half-open
  →（探测成功）→ closed；half-open 只放行少量探测，避免恢复瞬间打垮下游；
- **装饰器注入**：`breaker.NewStorage(rawStorage, b)` 实现 `StorageClient` 接口面，
  main 装配时替换即可——业务模块一行不改；
- **失败分类**（踩坑后补上的关键设计）：`Config.IsFailure` 允许把"对象不存在"
  这类**健康依赖的正常答复**排除在故障之外。实测踩过：MinIO 恢复后的 half-open
  探测因为返回 `NoSuchKey` 被判失败，熔断器立刻重新打开；
- **对外语义**：熔断打开 → `ErrStorageUnavailable` → HTTP **503**（依赖暂时不可用，
  客户端可退避重试），而不是 500（会被当成服务端 bug 计入错误率）；
- **启动不被依赖卡住**：bucket 检查失败只告警不阻塞启动——对象存储暂时不可用
  不应该让整个服务起不来（实测：MinIO 停着时服务直接启动失败）。

**实测（停掉 MinIO 后连续请求创建视频，该路径会 StatObject）**：

| 阶段 | 响应 | 延迟 |
|---|---|---|
| 前 5 次（真实故障） | 500 | 3.1 ~ 5.0s（底层超时） |
| 熔断打开后 | **503** | **0.7ms**（快速失败，不再调用 MinIO） |
| MinIO 恢复 + 冷却期后探测 | 400（业务答复） | 2.6ms，熔断器自动闭合 |

指标：`breaker_state{name}`、`breaker_opens_total`、`breaker_rejections_total`
（"省下了多少次必然失败的等待"的直接量化）、`breaker_probes_total{result}`。
已加入 `scripts/drill_failure_modes.sh` 第 6 步，可复现。

### 4.6 优惠券域（按 DomTok 的反面清单重新设计）

DomTok 有券但实现有 8 处硬伤（§2.1 第 7 条与商域报告 §3）：**没有发行量**、领取截止
从不校验、**领取次数由客户端传**、核销代码是死代码、门槛按单行而不是整单、
金额字段语义错位（`discount_amount` 装的是应付金额）、退款不处理券、券没有状态机。
结论是"功能缺失"而不是"简化"。

TokMall 重新设计并实现了 v1 满减券（`internal/coupon` + 迁移 000019），逐条对上：

| DomTok 的问题 | TokMall 的做法 |
|---|---|
| 没有发行量 | `total_quantity` + `issued_quantity`，领取时**条件更新**（`issued < total`）+ 数据库 CHECK 双保险 |
| 无每人限领 | `per_user_limit` + 领取事务内计数；并发 20 次领 2 张限领 → 恰好 2 次成功 |
| 领取截止不校验 | `valid_from/valid_to` 在**领取时**与**使用时**都校验；过期在读取时按当前时间判定 |
| 次数由客户端传 | 请求体只有 `coupon_id`；发行量、限领、优惠金额全部服务端决定 |
| 核销是死代码 | 下单事务内条件更新 `WHERE status='UNUSED'` + `RowsAffected=1` 校验；券状态机 UNUSED/USED/EXPIRED |
| 门槛按单行 | `DiscountFor` 只接受**整单商品金额**（v1 单 SKU 订单等价，多商品时语义已就位） |
| 金额字段语义错位 | 订单拆成 `original_amount / discount_amount / total_amount`，数据库 CHECK 保证 `total = original - discount` |
| 退款不处理券 | 全额退款成功 → 券退回 UNUSED（已过期则标 EXPIRED），并清空订单引用 |
| 无对账 | 新增三项对账检查：发行量守恒、券-订单一致、订单金额拆分 |

**并发证明**（`internal/coupon/postgres_integration_test.go`，真实 PostgreSQL）：

| 场景 | 结果 |
|---|---|
| 100 个用户并发抢 10 张券 | 恰好 10 成功、90 售罄；`issued_quantity` == 实际持券数 == 10 |
| 同一用户并发领 20 次（限领 2 张） | 恰好 2 次成功、18 次 `COUPON_ALREADY_CLAIMED`；被拒的领取回滚了发行量计数 |
| 绕过应用层裸 UPDATE 推高发行量 | 被数据库 CHECK `coupons_issued_not_exceed_total_check` 拒绝 |

**订单侧集成**（`internal/order/coupon_postgres_integration_test.go`）：
金额拆分正确、幂等重放不二次核销、退款退券（有效期内 UNUSED / 过期 EXPIRED）、
门槛不足 / 用别人的券 / 重复用同一张券都被拒绝。

**对账新增的三项**（`bin/reconcile` 从 6 项变 9 项）：`coupon_issuance`、`coupon_usage`、
`order_amount_split`。有意思的是：另外两种"更脏"的形态（已核销却没有订单、实付≠原价−优惠）
**被数据库 CHECK 直接挡住了，注入不进去**——约束把"能写坏的方式"收窄了，
对账只需覆盖约束管不到的部分。

**演练**：`scripts/drill_coupon.sh` 走完发券 → 领券防重 → 用券下单 → 支付 →
退款退券 → 九项对账，每一步都回查数据库事实。

---

## 5. 未落地但值得做的（含设计要点）

按建议顺序排列，每条都给出"要做什么、为什么、坑在哪"。

### 5.1 库存"可售量 = stock − locked"三段式

DomTok 的 `stock/lock_stock` 双列语义是对的（下单预占、支付实扣、超时释放），
错的只是**用 Redis 当事实源**。TokMall 可以在 PostgreSQL 内实现同一语义：

```sql
-- inventory 增加 locked 列；可售量 = quantity - locked
UPDATE inventory SET locked = locked + $1
 WHERE sku_id = $2 AND quantity - locked >= $1;   -- 预占（条件更新，不超卖）
UPDATE inventory SET quantity = quantity - $1, locked = locked - $1 WHERE sku_id = $2;  -- 实扣
UPDATE inventory SET locked = locked - $1 WHERE sku_id = $2;                            -- 释放
```

收益：把"下单即扣减"升级为"下单预占、支付实扣"，更贴近真实电商，
也自然解释"为什么未支付订单不影响可售量"。代价：订单状态机、超时/取消、
退款回补、对账（库存守恒公式变成 `quantity + locked` 的守恒）全部要跟着改，
属于**一次完整的领域重构**，需要单独的 ADR 与并发实验。

### 5.2 优惠券域 v2（v1 已落地，见 §4.6）

DomTok 的券实现有 8 处硬伤（无发行量、无截止校验、领取次数由客户端传、
核销是死代码、按单行而非整单门槛、金额字段语义错位、退款不处理券、无状态机）。
但**券本身是商城的标准能力**，做对了是很强的业务亮点：

- **领取防超发**：券模板带 `total_quantity / issued_quantity`，
  用 `UPDATE coupon SET issued = issued + 1 WHERE id=? AND issued < total` 条件更新，
  加 `(user_id, coupon_id)` 唯一约束实现"每人限领"；
- **核销**：`user_coupon.status`（UNUSED/USED/EXPIRED）+ 订单创建事务内条件更新，
  保证"一券一单只用一次"；
- **门槛按整单**：满减门槛比较订单商品总额（不是单行），折扣券按订单金额计算；
- **分摊**：订单级优惠按行金额比例分摊到 `order_items.discount_amount`，
  余数用最大余额法落到金额最大的行，保证 `Σ 分摊 = 订单优惠`；
- **退款退券**：全额退款且订单未完成时把券退回 UNUSED（记 `refund_from_status` 同款思路）；
- **对账**：把"券发行量 = 已发放 + 未发放""已用券数 = 使用券的订单数"加进 reconcile 检查项；
- **口径**：GMV 记实付还是原价？——按 ADR-017 的口径原则，**毛额记原价、优惠单列**。

### 5.3 把熔断扩展到 Redis 与下游 HTTP

本轮只把熔断接在对象存储上（`StatObject`/`DeleteObject` 是真正走网络的调用；
预签名是本地签名运算，不走网络——这也是个容易搞错的点）。
下一步的自然延伸：

- **Redis**：缓存读失败目前每次都要等连接超时（fail-open 但仍付超时代价），
  熔断后直接快速失败并走数据库；作用点是 feed 缓存与穿透防护的负缓存；
- **下游 HTTP**（未来拆服务后）：用同一套 breaker 包住 RPC 客户端；
- 注意**失败分类**：`redis.Nil`（key 不存在）不是故障，必须走 `Config.IsFailure` 排除，
  否则缓存未命中就能把熔断器打开。

### 5.4 两级缓存（L1 进程内 + L2 Redis）

TokMall 的 Feed 首页缓存目前只有 Redis（~11.3k QPS 封顶于 Redis GET + JSON 反序列化）。
加一层进程内 LRU（几百行、零依赖）可以把热 key 的读取从"一次网络往返"降到"一次内存查表"；
难点不在缓存本身，而在**失效**：

- 写路径删 L2 后 `PUBLISH cache:invalidate:{key}`，各实例订阅并删自己的 L1；
- L1 TTL 设短（秒级），作为"广播丢失"的兜底；
- 指标：`cache_l1_hit_total / l1_miss_total / l1_evictions_total / l1_invalidations_total`；
- 验收：压测对比 L1 开关的 QPS 与 p99；并发测试证明失效广播能在秒级内传播。

### 5.5 其他低成本项

- **Prometheus 录制规则 + 告警**：`job:tokmall_http_requests:rate5m`、
  `instance:tokmall_db_pool_wait_seconds:p95`、outbox 积压时长、对账差异 > 0 四条；
  docker-compose 里已有 prometheus 镜像（本地可跑）。
- **支付一次性令牌**：`POST /orders/{id}/pay-token` 发令牌（Redis 存，TTL 与订单一致），
  `POST /orders/{id}/pay` 必须带令牌，用 Lua `GET + DEL` 原子消费；
  防重放，且令牌可"回滚"（保留剩余 TTL）以便失败重试。
- **错误创建点抓栈**：TokMall 的哨兵错误是包级单例（创建于 init），
  需要改成"在 handler 判定为未知错误时抓栈"的形态才有意义，优先级较低。

---

## 6. 一句话结论（可直接用于面试）

> 我读过同类的分布式电商开源项目（DomTok，Kitex 微服务 + RocketMQ + ES 全家桶），
> 对比后我把它的**工程外壳**搬了过来（布隆过滤器防穿透、延迟消息做超时、
> CI 流水线、GORM 日志策略），但**交易内核一个都没抄**——
> 因为它的库存预扣不校验库存、支付状态存在 Redis 里当判据、金额用 float64、
> 状态更新没有条件更新，这四条恰好是 TokMall 已经用条件更新 + CHECK 约束 +
> Outbox + 对账解决掉的问题。
>
> 最有价值的一次借鉴是它的延迟消息：我用 Outbox 的 `available_at` 实现了同样的
> 延迟语义，但把数据库留作唯一事实源、消息只当加速器——**消息全丢，系统仍在
> 轮询间隔内收敛**。这是"引入中间件不等于提升可靠性"的一个具体例子。
