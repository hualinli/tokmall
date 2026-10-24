# 项目工作约定

## 项目与事实入口

TokMall 是 Hertz 短视频 Feed + 商城学习项目，当前为共享 PostgreSQL 的模块化单体，配套 React 前端。目标是通过可演进系统掌握分层、状态机、幂等、缓存、观测与恢复。模块不共享业务 model；跨模块只读通过各自 read model / SQL，调用接口定义在消费侧，由 `cmd/api/main.go` 装配。

文档按职责维护，不在这里复制里程碑流水账：

| 信息 | 事实入口 |
|---|---|
| 业务规则与未闭环项 | [业务](docs/business.md) |
| 实现、设计取舍与代价 | [架构](docs/architecture.md)、[main](cmd/api/main.go) |
| 运行／测试／迁移／排障 | [运维](docs/operations.md) |
| 性能数字、压测计划与方法 | [性能](docs/performance.md) |
| 未来工作 | [roadmap](docs/roadmap.md) |
| API / schema / 配置 | [OpenAPI](internal/api/openapi.yaml)、[migrations](migrations/)、[config](internal/config/config.go) |
| 面试解释与证据 | [面试清单](docs/interview.md) |

技术栈：Hertz/GORM/PostgreSQL/golang-migrate；Redis 缓存、限流与 Streams；RustFS；JWT；slog/Prometheus/Grafana；React/TanStack Router/Query/Vite/Tailwind。版本以 go.mod、web/package.json、锁文件与 CI 为准。schema 仅由迁移管理，**禁止 AutoMigrate**。

当前后端已有商品/库存/订单/退款/券、Outbox/日销投影/九项对账、全局/关注/热榜 Feed、媒体校验、对象清理、两级缓存/穿透防护、熔断、告警规则与 traceparent。Kafka、真实支付物流、完整 OTel、累计上传配额及部分前端接入尚未实现，详情见 roadmap。当前迁移到 000026；目录入口见根 README，避免另一份易过期的目录树。

## 改代码前必读

- **分层与错误**：handler HTTP DTO → service 领域参数/视图 → repo model；model 不出 service。
  repo 将 ORM 错误翻译为领域哨兵，service 分类并按业务/系统错误分级，handler 映射 BizError，未知返回500。
  用 `errors.Is/As` 保留错误链，业务拒绝不打 Error。列名/状态类型在 GORM 边界显式转 string。
- **权限与可见性**：身份来自认证中间件，资源归属在服务端验证；非作者与不存在用同一404。
  DELETED 视频详情对所有人不可见，未发布仅作者可见。当前商家能力主要是 owner ID 授权，不虚构 MERCHANT 角色门禁。
- **状态机**：视频表驱动允许迁移，数据库 `WHERE status=from` 最终防并发。
  订单履约使用独立 `fulfillmentTransitions`，不得复用含退款分支的完整状态图。
  全额退款仅允许 PAID/FULFILLING；COMPLETED 不接受申请，拒绝恢复申请前状态。
  发货写 auto_complete_at，轮询 SKIP LOCKED 到期自动 COMPLETED；拒绝退款退回 FULFILLING 时只顺延不清空，不用延迟事件。
  申请退款写入 respond_by。到期由 worker 逐笔锁订单后复用 approveRefund，来源为 TIMEOUT、decided_by 为空。
- **幂等与账本**：点赞/关注唯一关系 + ON CONFLICT；同幂等键不同业务事实必须冲突。
  下单指纹、支付身份、播放 event_id 均校验载荷一致；库存余额非负，流水业务键保证一次扣减/回补。
- **库存与退款**：订单与库存同库同事务；未发货退款回补，已发货退款不回补，不能冒充退货入库。
  商品/SKU/价格来自服务端，订单保留快照。跨模块库存写入仍存在于 order/product/inventory，DB 约束兜底。
- **券**：规则集中在 `coupon.DiscountFor`，门槛按整单原价；模板锁/条件发行量/用户限领防超发。
  商家券只用于同商家 SKU；`total = original - discount` 有 DB CHECK。
  取消、三条超时路径和成功退款都退券（有效 UNUSED、过期 EXPIRED）；订单保留历史引用，唯一索引只约束仍占券订单。
- **事件与对账**：事实与 Outbox 同事务，Enqueue 只接受 tx；投递至少一次，不宣称消息恰好一次。
  消费回执与投影同事务，无回执表示投影未生效（正常应用路径）；不能人工删已消费回执后重放。
  投影重建必须指定实际消费组、支付/退款事件回执齐全，并在维护窗口取得 NOWAIT 表锁，否则拒绝。
  库内 DEAD 与消费侧 DLQ Stream 区分。PENDING 包含未来 timeout，积压只看已到期事件。
  毛额统计所有曾支付订单，成功退款单列；改投影口径同步改 reconcile，退款申请不减少成交。
- **游标与 Feed**：时间排序使用 `(published_at,id)`，HMAC 游标表示上一页末项并带 rank_version。
  关注流是完整时间线，生产仅 TimeRanker；实验 HotRanker 未闭合游标，禁止配置/Service 上线。
  热榜独立 v2 分钟桶/冻结窗口，2000条上限、窗口TTL10分钟；游标含窗口/分数/ID，已有快照不重写。
  旧窗口到期拒绝游标；首页降级时间流后，后续沿用时间游标，不能中途切换排序。
- **缓存**：Cache-Aside + singleflight + double-check；共享结果不得被请求级个性化修改。
  全局 L1/L2 写后失效当前先删本地L1、再删L2、递增 `feed:version`；L1 TTL≤L2 TTL。
  当前 DEL/Bump/版本读取失败会扩大远端陈旧窗口，不能把版本探测间隔写成所有故障下的硬上界。
  关注缓存按用户失效和TTL收敛；发布/删除不会遍历粉丝key。商品卡/点赞数按请求批量装配，禁止N+1。
- **穿透**：负缓存只记录真实 DB miss，不能把不可见的未发布视频缓存为不存在。
  负缓存读取失败放行到数据库。本地布隆过滤器已删除，它不参与判定。
- **上传与清理**：PUT只授权 `staging/`，签名绑定长度/类型；封存检查实际大小并按源ETag复制最终key。
  票证锁行；视频/封面核销与创建同事务。RustFS副作用不能随PG回滚，final孤儿仍是已知边界。
  删除状态与 `object_delete_jobs` 同事务，后台失败重试，staging等原PUT到期后清理。
  媒体回写持视频行锁，不复活已删除对象；升级旧PUT需等原有效期过后再恢复服务。
- **媒体**：PENDING/PASSED/REJECTED/SKIPPED 区分；坏文件 REJECTED，工具缺失/关闭探测 SKIPPED。
  只有 PASSED/SKIPPED 可发布；下载/探测暂时失败延迟重试，取件排除DELETED。多worker可重复探测，但只接受一次结果。
- **安全配置**：JWT_SECRET / FEED_CURSOR_SECRET所有环境必填；生产显式凭证、不同强密钥和TLS。
  TRUSTED_PROXIES默认空，禁止0.0.0.0/0。Refresh用Redis原子Claim阻止并发双签发。
- **日志与观测**：用`slog.*Context`，ContextHandler注入request/trace/route/user并脱敏。
  指标标签用路由模板，禁止user_id/order_no高基数字段。GORM参数化日志、忽略正常not-found、慢SQL阈值200ms。
  改Prometheus规则后跑`go test ./internal/observability/`，记录规则遵循`level:metric:operations`。
  对象不存在/无效大小不计熔断故障，open对外503；存储不可用不阻塞启动。
- **门禁**：提交前 `make lint`；errorlint/nilerr/forcetypeassert是护栏，必要nolint须写理由。
  CI包括后端/契约/前端/E2E与独立Security，但默认测试须显式开启真实依赖开关；不把完整迁移回滚描述为已覆盖。

## 通用规则

- 不要为猜测出来、几乎不会发生的竞态或安全边界情况加复杂度。真觉得要加，先说清楚具体会怎么出错、概率多大、要多写多少代码，我同意了再做。不要悄悄扩大改动范围。
- 外部传进来的数据，只在入口那一层校验一次。往里传已经确定类型的值，内部不要反复做防御性检查。
- 不要提前抽只用一次的函数。只有会复用、包住了确实复杂的逻辑，或者它代表一个能起清楚名字的概念时才抽。
- 主函数只写正常流程，校验和细节放到它下面的小函数里。
- 注释只写不明显的约束和反直觉的行为，不给显而易见的代码写注释。
- 自动生成的文件不要手改，改源头后重新生成。
- 测试测真实实现，不要把业务逻辑抄一份进测试。尽量少用 mock。
- 按仓库现有的流程走，不要引入仓库没在用的工具或流程。

## 开发环境要点

- 标准端口PG5432、Redis6379、RustFS9000/9001；禁止额外创建5433实例。
- Docker凭证助手异常：用`DOCKER_CONFIG=$PWD/.docker-anon`，配置中的cliPluginsExtraDirs需指向本机Docker插件目录。
  `.docker-anon`已忽略；Redis使用compose固定的redis:8-alpine（Streams/XAUTOCLAIM/XPENDING 语义与7.4一致，已实测）。
- `make dev`启动；`make stop`只匹配bin/api与Vite但会强制终止；优雅停机需给明确PID发SIGTERM。
  不要`lsof -ti :8080 | xargs kill -9`，可能误杀其他应用。macOS无setsid，后台用nohup；压测用编译的`./bin/api`。
- 沙箱Go缓存：`GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gopath/pkg/mod GOCACHE=$PWD/.gocache GOSUMDB=off`。
- `/metrics`默认监听127.0.0.1:9091；启动会主动探测，出现`metrics endpoint is not reachable`要查监听与日志。
- docs/和AGENTS.md由Git跟踪，共6份文档，每个事实只写在负责它的那份里，其他处用链接；不要新建文档或重复状态表，历史全文从Git查看。
- 真实PG/Redis/RustFS测试需显式环境开关，见运维。默认Go测试跳过它们；历史QPS也不是当前容量承诺。

## 工作方式（Mentor模式）

少写具体代码，多教分层、错误、并发、性能与安全思路。先计划、用户审核、按步骤执行并解释原因；已有明确授权时在范围内推进。用户写代码后按P0→P1→P2 review，严重生产级盲区主动警告并给出工业级解法。用户写不出来时提供完整实现。避免顺从性陷阱，普通功能和修复更新对应文档，设计取舍写在架构文档对应主题的“为什么/代价”里，不为技术名词拆服务。任何子代理分工由当前用户授权或明确项目指令决定。
