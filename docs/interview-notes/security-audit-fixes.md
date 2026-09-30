# 安全审计与修复：从 P0 到 P2，以及一个 ORM 默认值陷阱

## 这一步交付了什么

`docs/project-audit-2026-10.md` 是一次完整的后端审计，`docs/remediation-plan.md` 是修复计划。
这一轮关掉了 BE-P0-1 与 BE-P1-1～P1-8，并逐条修复选定的 P2；BE-P1-9 作为已知边界记录在下文。
每一项都补了可复跑的回归测试，编号沿用审计报告；BE-P2-1 的取舍被固化成 ADR-020（不可见资源统一 404）。

## BE-P0-1：限流把客户端可控的 X-Forwarded-For 当成事实

- **机制**：限流 key 取 `ctx.ClientIP()`，而 Hertz v0.10.5 的默认可信代理列表是
  `0.0.0.0/0` 与 `::/0`——等于信任所有人。于是客户端自带的 `X-Forwarded-For` 被原样采信，
  每个请求换一个 XFF 就是一个新的限流桶。
- **影响**：登录爆破/撞库的按 IP 限流形同虚设（它与 BE-P1-1 叠加后基本归零）。
- **修复**：`cmd/api/main.go` 显式设置
  `h.SetClientIPFunc(app.ClientIPWithOption(app.ClientIPOptions{RemoteIPHeaders: [...], TrustedCIDRs: cfg.TrustedProxies}))`；
  `internal/config/config.go` 的 `parseTrustedProxies` 只接受 CIDR（强制配置者想清楚范围），
  `validate()` 拒绝 `ones == 0` 的网段并 fail fast。
- **为什么"没配代理就用 TCP 对端地址"是安全默认**：转发头是客户端可控输入，只有
  "最后一跳是我们已知的代理"时它才有资格参与来源判定；默认空列表 = 不解析任何转发头。

## BE-P1-1：账号限流 key 未规范化，加空白前缀就换桶

限流 key 用请求体里的原始标识，而业务查询会 `TrimSpace` 后照常命中同一账号——加一个前导空格
就能拿到新桶，10/min/账号的防爆破失效。修复是让提取器与业务查询共用**同一份**规范化：
`internal/middleware/ratelimit.go` 的 `AccountKeyExtractor` 调用 `NormalizeAccountKey`
（`strings.ToLower(strings.TrimSpace(...))`）。`ratelimit_test.go` 的
`TestAccountKeyExtractor_NormalizesIdentifier` 与 `TestNormalizeAccountKey` 锁死这条约定；
"限流 key 与业务查询必须用同一规则"写在函数注释里，避免下次又各写一份。

## BE-P1-2：超时订单仍可支付，以及一个事务写法的坑

`Pay` 原先只校验状态机与金额，不校验 `expires_at`；而关单 Worker 有吞吐上限、可以被关掉，
还使用 `SKIP LOCKED` 扫描（`internal/order/repository.go` 的 `ExpireNext`）——
正在被支付事务锁住的行会被它跳过。两者叠加会出现"支付被拒 + Worker 也跳过"，
订单永远停在 `PENDING_PAYMENT`。

修复是**懒过期**：在支付事务的锁内自己判定 `expires_at`，超时就地调用 `expireLockedOrder`
关单并回补库存，条件更新再带一个 `AND expires_at > ?` 兜底。

这里有一个必须讲清楚的事务陷阱：**在 `Transaction(func(tx) error)` 闭包里 `return ErrOrderExpired`
会把刚刚写下的关单与库存回补一起回滚**。所以代码用 `expiredDuringPay` 标记：
事务内提交副作用并正常返回，提交之后再返回业务失败给调用方。库里不留支付尝试记录、
库存回补一次、`ORDER_EXPIRED` 流水一条。
真实 PG 证据：`internal/order/postgres_integration_test.go` 的
`late payment is rejected without the expiry worker`（关掉 Worker 也拒绝）与
`payment and expiry choose one terminal state`（并发只有一个终态）。

## BE-P1-3：refresh 旋转是 check-then-act

原实现是 `IsBlocked` → 签发 → `Block`，两个并发刷新都能通过检查、各自拿到 token 对，
被窃取的 refresh token 在窗口内可以并行换取自己的 token。修复是把黑名单当**抢占**用：
`internal/user/blacklist.go` 的 `Claim` 就是 `SetNX`，`internal/user/service.go` 的 `Refresh`
先 `Claim(jti)`，返回 `false` 直接 401，成功后才签发。测试：`TestRefreshConcurrentReplayIssuesOneTokenPair`、
`TestRefreshRotationConsumesOldToken`、`TestRefreshClaimFailure`。

## BE-P1-4：singleflight 捕获 leader 的 ctx，把故障放大

执行闭包直接用了首个请求的 `ctx`，leader 断连/超时会把同一个错误广播给整批等待者——
缓存击穿保护变成故障放大器。修复在 `internal/feed/service.go`：
执行体绑定独立上下文

```go
loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), singleflightTimeout)
```

既不被首个请求的取消信号传染，也不会无限期占用（Redis/PG 挂住时有 3s 上界）。
首页缓存与关注 Feed 首页两条路径都改了。

## BE-P1-5 / BE-P1-6：超长输入打成 500，trace 还能被伪造

`play_events.session_id` 是 `VARCHAR(128)`、`trace_id` 是 `VARCHAR(64)`，而入口不校验长度，
超长值触发 PostgreSQL 22001 被兜底成 500；`trace_id` 又直接取 `requestid.Get(c)`，
而 requestid 中间件会原样回显客户端传来的 `X-Request-ID`，观测字段可以被任意伪造。

修复是把校验放在入口并统一成"不合法就存 NULL"（`internal/video/service.go`）：
`maxSessionIDLength = 128` / `maxTraceIDLength = 64`，`normalizePlayEventSessionID`
丢弃空白与超长值，`normalizeTraceID` 还要求 `uuid.Parse` 成功——只接受服务端中间件生成的
UUID v7。这样既没有 500 触发面，也保证 trace 是可信的观测事实。

## BE-P1-7：邮箱未规范化、未校验列宽

`mail.ParseAddress` 会接受 312 字符的地址，但 `users.email` 是 `VARCHAR(255)`：
"参数非法"以 500 的形式暴露；同时 `Alice@x.com` 与 `alice@x.com` 能注册成两个账号。
修复是 `NormalizeEmail`（`internal/user/service.go`）：取 `addr.Address`、去空白、统一小写、
限制 `maxEmailLength = 254` 且只允许一个 `@`，注册与登录共用它；
迁移 `000016_invariant_hardening.up.sql` 加 `lower(email)` 唯一索引作为数据库最后防线
（发现大小写重复时 `RAISE EXCEPTION` 让迁移失败并暴露脏数据，不静默改写）。
测试：`TestNormalizeEmail`、`TestRegisterNormalizesEmailAndLoginIsCaseInsensitive`。

## BE-P1-8：取关把所有错误映射成 500

`DELETE /users/{id}/follow` 的 handler 对任何错误都返回 500，契约声明的 400/404 永不出现，
"取关一个不存在的用户"被客户端和监控误判成服务端故障。修复是照 `follow` 补齐映射：
`ErrInvalidParam → 400`、`ErrUserNotFound → 404`，default 才记 `slog.Error` 并返回 500
（`internal/interaction/handler.go`）。

## P2 批量修复

| 条目 | 修复 | 位置 / 证据 |
|---|---|---|
| BE-P2-1 非作者删除返回 403 | 与"不可见一律 404"统一，非作者与不存在同错 | `internal/video/service.go` `Delete`；ADR-020 |
| BE-P2-3 ticket 核销与视频落库不在一事务 | 新增 `ConsumeTicketAndCreateVideo`，核销失败则视频不落库 | `internal/video/repository.go` |
| BE-P2-5 文本字段无长度上限 | title ≤200、description ≤2000、spec ≤200、关键字 ≤64、email ≤254 | `internal/product/service.go`、`internal/user/service.go` |
| BE-P2-6 LIKE 通配符未转义 | `escapeLikePattern` 转义 `\ % _`，配 `ILIKE ? ESCAPE '\'`；关键字另设长度上限 | `internal/product/repository.go`；`TestEscapeLikePattern` |
| BE-P2-11 下架商品仍给可购 SKU | 非 `ON_SALE` 商品详情返回空 SKU 列表，`ON_SALE` 只返回 ACTIVE 的 SKU | `internal/product/service.go` `GetProduct` |
| BE-P2-7 库存 change 无上界 | `maxStockChange = 1_000_000_000`，越界返回 400 而不是让 BIGINT 溢出成 500 | `internal/inventory/service.go` |
| BE-P2-8 `users.status` 全链路不校验 | `User.Usable()` 卡住登录与续签；`GetProfile` 对停用账号按不存在处理；互动列表 JOIN `status = 'ACTIVE' AND deleted_at IS NULL` | `internal/user/`、`internal/interaction/repository.go`；`TestLoginRejectsDisabledAccount`、`TestRefreshRejectsDisabledAccount` |
| BE-P2-10 非生产环境用内置默认密钥 | `JWT_SECRET` / `FEED_CURSOR_SECRET` 在所有环境都走 `requiredEnv`，无内置默认值 | `internal/config/config.go`；`TestLoadRequiresSigningSecretsInEveryEnvironment` |
| BE-P2-9 `/metrics` 监听 0.0.0.0 | 默认 `127.0.0.1:9091`，可用 `METRICS_ADDR` 指到内网；启动后主动探测端口，绑定失败会大声报错 | `internal/config/config.go`、`cmd/api/main.go`；`TestLoadValidatesMetricsAddr` |

## 另一个真实缺陷：GORM 零值绕过了 `DEFAULT 'ACTIVE'`

这是端到端演练抓到的、比任何设计都更像真实工作的一件事：`scripts/drill_commerce_loop.sh`
的演练里注册返回 201，紧接着用同一账号登录却 401。查库发现新用户的 `status` 是**空字符串**——
为了让"停用账号不能登录"生效而给 model 映射了 `Status` 字段，而
**GORM 的 `Create` 会把结构体字段（含零值）写进 INSERT**，空串绕过了 `DEFAULT 'ACTIVE'`；
空状态既不是 ACTIVE 也不是 DISABLED，登录一律被判为不可用。

三层修复（`docs/interview-walkthrough.md` §11 有完整排查过程）：

1. `Register` 显式写入 `Status: UserStatusActive`（`internal/user/service.go`），
   新用户不再依赖数据库默认值；model 上同时加 `default:ACTIVE`
   （`internal/user/model.go`），让 GORM 在零值时省略该列——两条防线，
   因为任何一条被将来重构掉，另一条仍能挡住；
2. 迁移 `000018_user_status_invariant.up.sql` 修历史空值并**恢复 `users_status_check` CHECK**——
   000002 曾把状态枚举约束整体移到应用层，但 `status` 与 `role` 不同：写错的后果是认证失效，
   必须由数据库兜底；
3. 真实 PostgreSQL 测试 `TestPostgresRegisterPersistsActiveStatus`
   （`internal/user/postgres_integration_test.go`）断言"库里存的确实是 ACTIVE"，
   并且用户名登录、含空白与大写的邮箱登录都能通过。

教训写在测试注释里：**fake repository 永远发现不了"ORM 与 DDL 默认值打架"这类问题**——
单测里 fake 会把 Status 填成 ACTIVE，测试全绿；只有真实数据库能暴露它。

## 已知边界

- BE-P1-9（预签名 PUT 不绑定实际大小、无配额）没有在本轮闭环，作为已知边界记录：
  需要换 `PresignedPostPolicy`（`content-length-range`）或上传后 `StatObject` 比对 + 每用户配额。
- 限流依赖 Redis，故障时 fail-open 放行并打 Warn：可用性优先，代价是 Redis 故障期间限流防线失效；认证类场景若要 fail-closed 需要另行配置。
- 文本长度上限目前是应用层校验，数据库列宽是 `TEXT`/`VARCHAR` 的最后防线，没有为每个文本字段都加 CHECK。
