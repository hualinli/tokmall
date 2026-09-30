# TokMall 项目审核报告（2026-10）

> 审计日期：2026-10-01。审计基线：`560dda1` 及其工作区版本，行号对应该版本。
> 本文是两份独立审计（后端安全与正确性、前端与契约层）的合并整理：只统一编号、排序和去重，不新增结论。
> 编号稳定：`BE-*` 后端、`FE-*` 前端、`CT-*` 契约、`E2E-*` 端到端覆盖与稳定性；修复计划按这些编号引用，见 [remediation-plan.md](remediation-plan.md)。

## 1. 一句话结论

项目主链（内容 → 挂商品 → 下单 → 支付 → 关单 → 履约 → 归因）在数据模型、幂等、状态机与真实 PostgreSQL 并发验证上已达到"局部生产级"：五类幂等键、条件状态更新、DB CHECK 兜底和 500 并发/100 库存实验都真实存在且已被审计核对通过；但整体尚未收口，审计确认后端 1 个 P0、9 个 P1、12 个 P2，前端 1 个 P0、6 个 P1、11 个 P2，契约 7 条问题。最大的三类风险是：**（1）安全护栏可被绕过**——限流 IP 与账号两个维度同时可伪造、非 production 环境存在可用默认 JWT 密钥、`/metrics` 无鉴权、`DISABLED` 用户不受任何限制；**（2）契约宣称的保障强度高于实际**——route-sync 只比路径、49 个 operation 无 `operationId`、文档写 limit 上限 100 而实际 50、429 完全未文档化、类型文件未入库导致契约漂移在 Git 层不可见；**（3）正确性依赖时序运气与运维纪律**——`Pay` 不校验 `expires_at`、refresh 旋转非原子、singleflight 共享 leader 的 ctx、关注流缓存不随视频删除失效、ticket 核销与视频落库不同事务。距离"可作为面试作品"缺的不是功能数量，而是三样东西：**售后/退款闭环未上线、支付后没有可靠事件分发、没有任何对账/业务指标/CI/下单压测/故障演练**——即现在讲得出功能，但证不了正确性。

## 2. 审核范围与方法

| 维度 | 审计对象 | 方法 | 未覆盖 |
|---|---|---|---|
| 后端安全与正确性 | `cmd/api`、`internal/**`（34 个非测试 Go 文件）、`migrations/000001~000014`、`scripts/` | 逐文件读实现；核对依赖库源码（hertz v0.10.5、gorm v1.31.2、minio-go v7.2.1、hertz-contrib/requestid v1.1.0）；本地实验（`net/mail` 行为） | 未做渗透测试、未做长期压测 |
| 前端与契约 | `web/**`、`internal/api/openapi.yaml`、`cmd/api/main_test.go` | 逐文件阅读 + 实跑 `tsc`、`oxlint`、`vitest`、`vite build`、`openapi-typescript`、`tsr generate`、`go test ./cmd/... -run TestOpenAPISync`；对 `openapi.yaml` 做两次受控变异实验后还原（`git diff` 为空） | `pnpm test:e2e` 未实跑（当时 5173/8080 无监听） |

审计期间实跑结果（用于判断"今天是绿是红"）：

| 命令 | 实测结果 |
|---|---|
| `go test ./cmd/... -run TestOpenAPISync` | PASS（0.4s） |
| `cd web && pnpm gen && pnpm typecheck` | 0 error（生成耗时 78.7ms） |
| `cd web && pnpm typecheck`（不 gen） | **18 error**（见 FE-P1-5） |
| `cd web && pnpm lint`（oxlint） | 0 error / 18 warning（全部是 `react/only-export-components`，路由文件导出组件所致） |
| `cd web && vite build` | 成功（848ms，main 产物 243KB） |
| `cd web && pnpm test`（vitest） | 通过（`passWithNoTests`，单测文件数为 0） |
| `cd web && pnpm test:e2e` | 未实跑（依赖后端 + PG/Redis/MinIO） |

快照与边界：

- `web/`、`cmd/`、`internal/api/` 在审计期间无并发改动；`internal/order/` 有并发进行中的退款开发（新增 `internal/order/refund_*.go`、`model.go` 增加 `REFUND_REQUESTED/REFUNDED`、`migrations/000015_order_refund.*`），相关结论已单独标注（CT-7）。
- 本次审计只读，未修改任何项目文件；`openapi.yaml` 的两次变异实验已还原。
- 行号来自两份审计报告的原始记录，未在本文中重新定位。审计之后代码会继续演进（例如限流与退款正在被修复），行号可能漂移；条目的关闭状态以 [remediation-plan.md](remediation-plan.md) 的验收证据为准，不以行号是否仍然命中为准。

## 3. 问题清单

### 3.1 后端（BE-*）

**P0 严重（安全 / 数据损坏 / 崩溃）**

#### BE-P0-1 限流 IP 维度采信客户端 `X-Forwarded-For`，可逐请求绕过

- **位置**：`internal/middleware/ratelimit.go:94`；`cmd/api/main.go:99-106`
- **现象**：限流的 IP 维度用 `ctx.ClientIP()`，而 Hertz v0.10.5 默认把 `0.0.0.0/0` 与 `::/0` 全部当作可信代理，客户端自带的 `X-Forwarded-For` 被原样采信。
- **证据**：

  ```go
  // internal/middleware/ratelimit.go:93-95
  func rateLimitKey(ctx *app.RequestContext, rule RateLimitRule) string {
  	dimension, id := "ip", ctx.ClientIP()
  ```

  ```go
  // hertz@v0.10.5/pkg/app/context.go:86-92：defaultTrustedCIDRs 含 0.0.0.0/0 与 ::/0
  var defaultTrustedCIDRs = []*net.IPNet{
  ```

  `context.go:1305 ClientIP()` → `context.go:148 validateHeader()` 从右往左找"不可信"地址，全部可信时返回最左项，即客户端可控值。

- **影响**：所有按 IP 的限流（登录 100/min、读接口 6000/min）可被任意客户端逐请求伪造绕过；与 BE-P1-1 叠加后，登录爆破/撞库的限流实际只剩 bcrypt 的算力成本。爬虫与洪峰防护失效。它本身不构成认证绕过，故列 P0 而非更高。
- **修复方向**：`server.WithClientIPFunc(...)` 只信任真实入口网段，或直接使用 `ctx.RemoteAddr()`；把可信代理范围写进部署文档。

**P1 正确性与并发**

#### BE-P1-1 账号维度限流 key 未规范化，加空白前缀即可换桶

- **位置**：`internal/middleware/ratelimit.go:50`
- **现象**：账号维度限流 key 用请求体里的原始标识（未 trim），而业务查询会 `TrimSpace` → 加任意空白前缀即可得到新桶，10/min/账号形同虚设。
- **证据**：

  ```go
  // internal/middleware/ratelimit.go:50-53
  for _, account := range []string{payload.Identifier, payload.Email, payload.Username} {
  	if account != "" {
  		return "account", account, true
  ```

  ```go
  // internal/user/service.go:253（register 见 :203 同样 TrimSpace）
  u, err = s.repo.FindByUsername(ctx, strings.TrimSpace(p.EmailOrUsername))
  ```

- **影响**：与 BE-P0-1 叠加后，认证接口的限流基本失效。
- **修复方向**：extractor 与 service 共用同一份规范化（`strings.TrimSpace` + 统一小写），或对账号 key 做 HMAC 归一。

#### BE-P1-2 `Pay` 不校验 `expires_at`，超时订单仍可支付

- **位置**：`internal/order/repository.go:165`
- **现象**：`Pay` 只校验状态机，不校验 `expires_at`：超时但未被 Worker 扫到的订单仍可支付。
- **证据**：

  ```go
  // internal/order/repository.go:165-170
  if !canOrderTransition(order.Status, StatusPaid) {
  	return ErrOrderStateConflict
  }
  if order.TotalAmount != cmd.Amount {
  	return ErrPaymentAmountMismatch
  }
  ```

  Worker 吞吐有硬上限：`internal/order/expiry_worker.go:24-40` 每轮最多 `batchSize` 条；`internal/config/config.go:42-44` 默认 100 条/5s = 20 单/秒，且 `ORDER_EXPIRY_WORKER_ENABLED=false` 时永不关单。

- **影响**：`expires_at` 语义不可依赖；积压或关停 Worker 时"超时订单"可继续支付，与设计文档的状态机窗口不一致。
- **修复方向**：在 `Pay` 的锁内加 `AND expires_at > now()`（条件更新包含该谓词），并让 Worker 支持批量循环。

#### BE-P1-3 refresh 旋转是 check-then-act，并发刷新双签发

- **位置**：`internal/user/service.go:346`
- **现象**：refresh 旋转是 `IsBlocked` → `Block` 的 check-then-act，并发使用同一 refresh token 刷新会双签发；原子原语已存在但用错顺序。
- **证据**：

  ```go
  // internal/user/service.go:346-357
  blocked, err := s.blacklist.IsBlocked(ctx, claims.ID)
  ...
  if err := s.blacklist.Block(ctx, claims.ID, remaining); err != nil {
  ```

  `internal/user/blacklist.go:31` 的 `Block` 就是 `SetNX`，可返回是否抢占成功。

- **影响**：被窃取的 refresh token 在窗口内可并行换取自己的 token 对；代码注释已承认该限制。
- **修复方向**：把黑名单当 claim 用——`SetNX` 返回 false 直接 401，成功后再签发。

#### BE-P1-4 singleflight 闭包捕获首个请求的 ctx，leader 失败会扩散

- **位置**：`internal/feed/service.go:400`
- **现象**：闭包捕获"首个请求"的 ctx，leader 取消/超时会把错误扩散给同批所有等待者。
- **证据**：

  ```go
  // internal/feed/service.go:400-407
  v, err, _ := s.group.Do(firstPageKey, func() (any, error) {
  	cached, err := s.cache.Get(ctx)
  	...
  	result, err := s.loadPage(ctx, nil, defaultLimit)
  ```

  `internal/feed/service.go:264-270` 的 `followingFirstPage` 同构。

- **影响**：缓存击穿保护在 leader 断连时变成故障放大器：整批并发请求一起 500，而 singleflight 不缓存错误。
- **修复方向**：闭包内用 `context.WithoutCancel(ctx)` / 独立超时 context，或改 `DoChan` 由各调用方各自等待。

#### BE-P1-5 `session_id` 无长度校验，超长触发 PG 22001 被兜成 500

- **位置**：`internal/video/service.go:511`
- **现象**：`session_id` 无长度校验，而列为 `VARCHAR(128)` → 超长触发 PG 22001，被兜成 500 而非 400。
- **证据**：

  ```go
  // internal/video/service.go:511-516
  func normalizePlayEventSessionID(sessionID *string) *string {
  	if sessionID == nil || *sessionID == "" {
  		return nil
  	}
  ```

  ```sql
  -- migrations/000006_create_play_events.up.sql:9
    session_id VARCHAR(128),
  ```

  `internal/video/handler.go:33-35` 只有可空约束；`internal/video/repository.go:174-186` 的 `translatePlayEventError` 不认 22001。

- **影响**：任意登录用户可用一个超长 `session_id` 让 `/videos/{id}/play` 稳定 500（其幂等重试也一并失败）。
- **修复方向**：入口校验 `len(session_id) <= 128`（`trace_id <= 64`），并把 22001 翻译成领域哨兵 → 400。

#### BE-P1-6 入站 `X-Request-ID` 被原样回显并写入 `trace_id`

- **位置**：`internal/video/handler.go:194`
- **现象**：客户端传入的 `X-Request-ID` 被 requestid 原样回显并写入 `trace_id VARCHAR(64)`，超长即 500，且观测字段可被伪造。
- **证据**：

  ```go
  // internal/video/handler.go:194-196
  if rid := requestid.Get(c); rid != "" {
  	traceID = &rid
  }
  ```

  ```go
  // hertz-contrib/requestid@v1.1.0/requestid.go:50-59
  rid := c.Request.Header.Get(string(cfg.headerKey))
  if rid == "" { rid = cfg.generator(ctx, c) }
  ```

  ```sql
  -- migrations/000006_create_play_events.up.sql:11
    trace_id VARCHAR(64),
  ```

- **影响**：同上的 500 触发面；trace 可被任意值污染，排障时误导定位。
- **修复方向**：`trace_id` 只接受服务端生成值（忽略入站头，或校验为 UUID 且 ≤64），或在入口截断。

#### BE-P1-7 注册邮箱未规范化、未校验列宽

- **位置**：`internal/user/service.go:199`
- **现象**：注册邮箱只用 `mail.ParseAddress` 校验，不取规范化地址、不校验列宽 → 312 字符邮箱通过校验后 INSERT 失败 → 500；大小写不同的同一邮箱可注册两个账号。
- **证据**：

  ```go
  // internal/user/service.go:199-202
  _, err := mail.ParseAddress(p.Email)
  if err != nil {
  	return nil, ErrInvalidParam
  }
  ```

  ```go
  // internal/user/service.go:217-219
  user := User{ Email: p.Email, Username: username, ... }
  ```

  本地实测 `mail.ParseAddress(strings.Repeat("a",300)+"@example.com")`：err=nil，Address 长度 312；`users.email VARCHAR(255)` 见 `migrations/000001_create_users.up.sql:3`。

- **影响**：400 语义变 500；`Alice@x.com` / `alice@x.com` 成为两个账号，与唯一约束意图冲突。
- **修复方向**：用 `addr.Address` + `ToLower(TrimSpace(...))` 存储，加长度上限（≤254），并用 `lower(email)` 唯一索引兜底。

#### BE-P1-8 `DELETE /users/{id}/follow` 把所有错误映射成 500

- **位置**：`internal/interaction/handler.go:157`
- **现象**：取关把**所有**错误映射成 500，契约声明的 400/404 永不返回。
- **证据**：

  ```go
  // internal/interaction/handler.go:157-161
  result, err := h.service.Unfollow(ctx, h.userID(c), followeeID)
  if err != nil {
  	response.Error(c, response.ErrInternal)
  	return
  }
  ```

  对比 `follow` 的 `internal/interaction/handler.go:136-148`；`internal/api/openapi.yaml:524-529` 声明 400/404。

- **影响**：取关不存在用户/非法 id 返回 500，客户端与监控误判为服务端故障，且错误未记日志。
- **修复方向**：照 `follow` 补齐 `ErrInvalidParam→400`、`ErrUserNotFound→404`，default 才 500 并 `slog.Error`。

#### BE-P1-9 预签名 PUT 不绑定实际大小，且无配额

- **位置**：`internal/video/service.go:173`
- **现象**：`file_size` 只校验"声明值"，预签名 PUT 不绑定实际大小，且无配额 → 任意大小上传/存储滥用。
- **证据**：

  ```go
  // internal/video/service.go:176-179
  case "video/mp4", "video/webm", "video/quicktime":
  	if fileSize < 1<<20 || fileSize > 512<<20 {
  		return nil, ErrInvalidParam
  	}
  ```

  ```go
  // internal/storage/storage.go:60-61 → minio-go PresignedPutObject → presignURL(..., nil, nil)
  url, err := c.mc.PresignedPutObject(ctx, c.bucket, objectKey, c.uploadExpiry)
  ```

  `internal/video/service.go:233-235` 只判 `size == 0`，从不与声明值比对；`minio-go@v7.2.1/api-presigned.go:90-94` 签名不含 content-length 条件。

- **影响**：任意注册用户可无限申请 ticket 并直传任意大小对象，撑爆对象存储或产生费用。
- **修复方向**：改用 `PresignedPostPolicy`（policy 加 `content-length-range` 与 content-type 条件），或上传后 `StatObject` 比对并删除超额对象，另加每用户配额。

**P2 质量与一致性**

#### BE-P2-1 `DELETE /videos/{id}` 对非作者返回 403，可探测视频存在性

- **位置**：`internal/video/service.go:315`
- **现象**：删除他人视频返回 403 而非常规 404，可探测他人（含已删除/未发布）视频存在性。
- **证据**：

  ```go
  // internal/video/service.go:315-317
  if v.AuthorID != userID {
  	return ErrForbidden
  }
  ```

  `Get`/`Publish` 一律 `ErrNotFound`（`:281-288`、`:579-584`）；OpenAPI `:406-407` 与 `internal/video/service_test.go:477` 已把 403 固化。

- **影响**：BIGSERIAL 视频 ID 可枚举，可判定 DELETED/BLOCKED 视频是否存在，与"不可见一律 404"约定冲突。
- **修复方向**：明确取舍——统一改 404 并同步 OpenAPI/测试，或在文档显式记录 403 为有意例外。

#### BE-P2-2 直接解引用 `*last.PublishedAt`，缺 DB 不变量兜底

- **位置**：`internal/feed/service.go:492`
- **现象**：代码直接解引用 `*last.PublishedAt`，而 `PUBLISHED ⇒ published_at IS NOT NULL` 无 DB 约束兜底 → 脏数据时 panic（被 recovery 兜成 500）。
- **证据**：

  ```go
  // internal/feed/service.go:490-492
  last := videos[len(videos)-1]
  next, err := s.cursor.Encode(Cursor{
  	PublishedAt: *last.PublishedAt,
  ```

  ```go
  // internal/feed/ranker.go:50-51（解引用，nil 即 panic）
  if !out[i].Video.PublishedAt.Equal(*out[j].Video.PublishedAt) {
  ```

  `migrations/000003_create_videos.up.sql` 无对应 CHECK；当前写入路径 `internal/video/repository.go:65-80` 恰好维持该不变量。

- **影响**：人工 SQL 或未来迁移写入不一致数据即触发首屏翻页 500（非进程崩溃，但需修数据才恢复）。
- **修复方向**：迁移补 `CHECK (status <> 'PUBLISHED' OR published_at IS NOT NULL)`；代码侧对 nil 候选跳过或返回领域错误。

#### BE-P2-3 ticket 核销与视频落库不在同一事务

- **位置**：`internal/video/service.go:236`
- **现象**：核销成功而建行失败时凭证被烧掉。
- **证据**：

  ```go
  // internal/video/service.go:236-243 → 之后才 :261 CreateVideo
  err = s.repo.ConsumeTicket(ctx, userID, p.ObjectKey)
  ...
  if err := s.repo.CreateVideo(ctx, v); err != nil {
  ```

- **影响**：一次瞬时 DB 故障就让已上传成功的对象作废（用户体验损失 + 存储浪费）。
- **修复方向**：核销与建行放同一事务，或"先建行、后核销"并在核销失败时回滚。

#### BE-P2-4 关注 Feed 缓存不随视频删除/发布失效

- **位置**：`internal/feed/cache.go:139`
- **现象**：关注 Feed 缓存只在 follow/unfollow 时失效，视频删除/发布不会失效任何 follower 的缓存。
- **证据**：

  ```go
  // internal/feed/cache.go:139-143
  func (c *RedisFollowingFeedCache) InvalidateFollowingFeed(ctx context.Context, followerID int64) error {
  	...
  	return c.client.Del(ctx, followingCacheKeys(followerID)...).Err()
  }
  ```

  全仓唯一调用点是 `internal/interaction/service.go:157`；`internal/video/service.go:85` 只失效全局 `feed:first_page`。

- **影响**：TTL（默认 60s）内关注流仍展示已删除视频（MinIO 对象已删，卡片死链）；"先删缓存后被旧值回填"的 cache-aside 竞态也会让新关注者看到空时间线至多 60s。
- **修复方向**：视频状态变更时按 follower 集合失效，或给关注流加短 TTL + 写路径 bump 版本号；至少记录为已知取舍。

#### BE-P2-5 商品/用户/SKU 文本字段无长度上限

- **位置**：`internal/product/service.go:119`
- **现象**：商品 `title`/`description`、用户 `bio`、SKU `spec` 无长度上限（仅受 Hertz 默认 4MB body 限制）。
- **证据**：

  ```go
  // internal/product/service.go:119-123
  title := strings.TrimSpace(p.Title)
  if title == "" {
  	return nil, ErrInvalidParam
  }
  ```

  ```sql
  -- migrations/000007_create_commerce_core.up.sql:4-5（均为 TEXT）
      title       TEXT NOT NULL,
      description TEXT NOT NULL DEFAULT '',
  ```

- **影响**：单行可达数 MB，列表/详情与 Feed 商品卡缓存被放大，存储与带宽可被滥用。
- **修复方向**：与视频标题一致加显式上限（title ≤200、description ≤2000、bio ≤500、spec ≤200）。

#### BE-P2-6 搜索关键字未转义通配符且无长度上限

- **位置**：`internal/product/repository.go:82`
- **现象**：搜索关键字直接拼进 `ILIKE` 通配串，`%`/`_` 未转义且无长度上限 → 全表扫描放大器。
- **证据**：

  ```go
  // internal/product/repository.go:81-83
  if filter.Keyword != "" {
  	q = q.Where("title ILIKE ?", "%"+filter.Keyword+"%")
  }
  ```

- **影响**：`keyword=%` 使所有商品命中；前置通配符本就无法走索引，长关键字进一步放大 CPU/IO（参数化本身无注入风险）。
- **修复方向**：转义 `\ % _` 并按长度截断（如 ≤64），必要时改 `pg_trgm` 索引或全文检索。

#### BE-P2-7 库存 `change` 无上界，溢出报 22003 被兜成 500

- **位置**：`internal/inventory/service.go:68`
- **现象**：`change` 无上界，`quantity + change` 在 BIGINT 上溢出时 PostgreSQL 报 22003 → 500（具体错误码需在真实 PG 上复核）。
- **证据**：

  ```go
  // internal/inventory/service.go:68
  if merchantID <= 0 || productID <= 0 || skuID <= 0 || change == 0 || idempotencyKey == "" || len(idempotencyKey) > maxIdempotencyKeyLength {
  ```

  ```go
  // internal/inventory/repository.go:107-110
  res := db.Model(&Inventory{}).
  	Where("sku_id = ? AND quantity + ? >= 0", tx.SkuID, tx.Change).
  ```

- **影响**：商家自伤型 500（无跨用户影响），但混淆"参数非法"与"服务端故障"。
- **修复方向**：加业务上界（如 `|change| ≤ 1e9`）并在 service 层返回 400。

#### BE-P2-8 `users.status` 全链路不校验

- **位置**：`internal/user/service.go:249`
- **现象**：`users.status`（ACTIVE/DISABLED）全链路不校验：DISABLED 用户仍可登录/刷新/调用接口，而 interaction 视其为不存在。
- **证据**：

  ```go
  // internal/user/repository.go:59-67（FindByID/Username/Email 都不含 status 条件）
  if err := r.db.WithContext(ctx).Where(string(col)+" = ?", value).First(&u).Error; err != nil {
  ```

  ```go
  // internal/interaction/repository.go:130-131（同一张表按 ACTIVE 过滤）
  Where("id = ? AND deleted_at IS NULL AND status = ?", userID, activeUserStatus)
  ```

- **影响**：运维置 DISABLED 不产生任何效果（安全护栏失效）；`GET /users/:id` 对 DISABLED 用户 200，而 follow/列表按 404/不存在处理，语义不一致。
- **修复方向**：登录/刷新/鉴权统一加 `status='ACTIVE' AND deleted_at IS NULL`，并让公开资料与 interaction 采用同一可见性规则。

#### BE-P2-9 `/metrics` 监听 `0.0.0.0:9091` 且无认证

- **位置**：`cmd/api/main.go:101`
- **现象**：`/metrics` 监听 `0.0.0.0:9091` 且无认证、无绑定白名单。
- **证据**：

  ```go
  // cmd/api/main.go:101-105
  server.WithTracer(hertzmonitor.NewServerTracer(
  	":9091", "/metrics",
  	hertzmonitor.WithRegistry(metricsRegistry),
  ```

- **影响**：对外泄露路由级 QPS/延迟、DB 连接池与运行时指标，为攻击者提供指纹与容量信息。
- **修复方向**：绑定内网地址或前置鉴权/网络策略，并标注该端口不应暴露公网。

#### BE-P2-10 非 production 环境使用内置默认 JWT 密钥

- **位置**：`internal/config/config.go:18`
- **现象**：非 production 环境使用内置默认 JWT 密钥（`dev-only-jwt-secret`），误配 `APP_ENV=development` 即可伪造任意用户 token。
- **证据**：

  ```go
  // internal/config/config.go:18 与 :200
  defaultJWTSecret = "dev-only-jwt-secret"
  ...
  JWTSecret: envString("JWT_SECRET", defaultJWTSecret),
  ```

  `internal/config/config.go:271-296` 对 production 有严格护栏：≥32 字符、禁用已知占位密钥、与 cursor secret 不同。

- **影响**：护栏完全依赖 `APP_ENV` 正确；用 development 跑公网实例则认证体系即刻失效。
- **修复方向**：所有环境都强制显式提供 `JWT_SECRET`/`FEED_CURSOR_SECRET`（仅测试注入默认值），或对默认密钥打启动 WARN 并拒绝非 loopback 绑定。

#### BE-P2-11 `GET /products/{id}` 对 OFF_SALE 商品仍可见

- **位置**：`internal/product/service.go:157`
- **现象**：只对 DELETED 返回 404，OFF_SALE 商品及其在售 SKU 对匿名用户完全可见，与搜索只暴露 ON_SALE 的语义不一致（需确认是否刻意）。
- **证据**：

  ```go
  // internal/product/service.go:157-159
  if product.Status == ProductStatusDeleted {
  	return nil, ErrNotFound
  }
  ```

  同文件 `:300-307` 注释写"OFF_SALE/DELETED 对用户不可见——可见性判断收敛在 service"。

- **影响**：下架商品仍可被直接访问详情并作为在售展示；两处可见性规则需人工对齐，未来易改错。
- **修复方向**：确认产品语义；若"下架即不可浏览/购买"，`GetProduct` 也过滤 OFF_SALE（或返回带 status 的只读视图并明确文档）。

#### BE-P2-12 库存写入路径分散在三个模块

- **位置**：`internal/order/repository.go:95`
- **现象**：order/product 各自直接 UPDATE/INSERT `inventory`，绕过 inventory 模块的幂等与非负语义，只能靠 DB CHECK 兜底。
- **证据**：

  ```go
  // internal/order/repository.go:95-101（order 自己的扣减 SQL）
  stock := db.Model(&inventoryRow{}).
  	Where("sku_id = ? AND quantity >= ?", cmd.SKUID, cmd.Quantity)
  ```

  ```go
  // internal/product/repository.go:106-109（product 直接插库存行）
  return db.Exec("INSERT INTO inventory (sku_id, quantity, version) VALUES (?, 0, 0)", s.ID).Error
  ```

- **影响**：三处不变量（`quantity >= 0`、`(sku_id,reason,biz_id)` 唯一、SKU 必有库存行）靠约定同步，任一改动都可能静默漂移；`migrations/000011` 的 CHECK/唯一约束是唯一硬兜底。
- **修复方向**：收口为单一库存写入口（inventory 暴露供 order 事务内调用的接口），或把不变量下沉为 DB 触发器，禁止跨模块直接写对方拥有的表。

### 3.2 前端（FE-*）

**P0**

#### FE-P0-1 渲染期调用 `crypto.randomUUID()`，非安全上下文商品页整页崩溃

- **位置**：`web/src/routes/product.$id.tsx:29`
- **现象**：渲染期调用 `crypto.randomUUID()`，在非安全上下文（`http://` + 非 localhost）直接抛 TypeError，商品页整页崩溃。
- **证据**：

  ```tsx
  const orderKey = useRef({ key: crypto.randomUUID(), fingerprint: '' })
  ```

  `useRef` 的实参在每次渲染时都会求值；`crypto.randomUUID` 只在 secure context（https / localhost / 127.0.0.1）存在。

- **影响**：项目 e2e 明确按移动端视口设计，用手机通过局域网 IP（`http://192.168.x.x:5173`）联调时商品详情页整页白屏/走错误边界（`__root.tsx:14`），**购买闭环彻底不可用**；此路径在 localhost 下不会暴露，属于"开发环境永远测不到"的类型。
- **修复方向**：懒初始化（`useRef<{key:string;fingerprint:string}|null>(null)` + 首次使用时赋值），或统一用一个带 fallback 的 uuid 工具。

**P1**

#### FE-P1-1 关注成功后计数 +2，且改错字段

- **位置**：`web/src/features/interaction/follow-button.tsx:52-54` 与 `:88-96`
- **现象**：关注成功后计数被 +2，且改错字段（`following_count` 应为 `followers_count`）。
- **证据**：

  ```tsx
  onSuccess: (data) => { if (data) updateProfileCache(queryClient, userId, data.following === true) },
  ...
  following_count: Math.max(0, old.following_count + (following ? 1 : -1)),
  ```

  `onMutate`（42-46 行）已经调过一次 `updateProfileCache`，`onSuccess` 又调一次 → 同方向应用两次。字段语义也有误：`internal/user/repository.go:72-93` 的 `FollowCounts` 明确 `following` = 我关注了多少人、`followers` = 多少人关注我；关注别人应改**对方主页的 followers_count**。对照 `like-button.tsx:33` 的注释（"onSuccess 无需再改缓存（避免双倍计数）"），说明这是曾经踩过、但 follow 侧漏改的坑。

- **影响**：用户主页"关注数"每次关注 +2 / 取关 -2，粉丝数不动；`user.$id.tsx:73-75` 直接展示这两个数字，5s staleTime + 不重挂载 → 错误数值会一直停留。
- **修复方向**：`updateProfileCache` 改为幂等赋值（传入目标布尔值 + 只在 onMutate/onError 做增量），并把增量落到 `followers_count`；补一条单测。

#### FE-P1-2 React Query key 不匹配，作品网格点赞乐观更新是静默 no-op

- **位置**：`web/src/features/interaction/like-button.tsx:57-58` vs `web/src/features/user/user-works.tsx:16`
- **现象**：两处 query key 不匹配，作品网格的乐观更新静默失效。
- **证据**：

  ```tsx
  updateFeedResultCount(queryClient, ['user-videos'], videoId, delta)   // like-button.tsx:58
  queryKey: ['user-videos', String(userId)],                            // user-works.tsx:16
  ```

  `setQueryData` 要求 key **精确匹配**（`invalidateQueries` 才是前缀匹配）；`['user-videos']` 在缓存中不存在，且 updater 在 `old === undefined` 时直接 `return old`（`like-cache.ts:19`），所以既不报错也不生效。

- **影响**：在作者主页作品网格里点赞，数字不会变（详情/Feed 会变，行为不一致）；错误回滚同样无效（无害但掩盖问题）。`video.$id.tsx:62`、`me.tsx:87` 用 `invalidateQueries({queryKey:['user-videos']})` 是对的，说明只有这一处写错。
- **修复方向**：把 `userId` 一起传进 LikeButton，或在 like-cache 里遍历匹配前缀；更稳妥的是统一封装 queryKey 工厂函数。

#### FE-P1-3 token 存 localStorage，多标签页各自轮换导致互踢下线

- **位置**：`web/src/lib/auth-store.ts:38-46`、`web/src/lib/auth-lifecycle.tsx:18-26`
- **现象**：token 存 localStorage（XSS 可取 7 天 refresh token），且每标签页各自 14min 轮换 → 多标签互踢下线。
- **证据**：

  ```ts
  { name: 'tokmall-auth', partialize: (s) => ({ accessToken, refreshToken, user }) }  // 默认 localStorage
  ...
  const timer = setInterval(() => { refreshAccessToken().catch(() => {}) }, 14 * 60_000)
  ```

  后端是 refresh 轮换 + 旧 token 拉黑（ADR-011）。两个标签页各持同一 refresh token：A 刷新成功后 B 的 token 已被拉黑 → B 的 `doRefresh` 失败 → `clear()`（`auth-refresh.ts:31-35`）→ B 被静默登出；若 B 在 `/me`（`auth-lifecycle.tsx:28-33`），还会被弹到 `/login`。

- **影响**：多标签场景必然掉线；`refreshToken` 一旦因 XSS 泄露即可长期换取 access token（无 HttpOnly Cookie 缓解）。
- **修复方向**：短期至少用 `BroadcastChannel`/`storage` 事件同步 token 与登出；中期把 refresh token 收到 `HttpOnly + SameSite` Cookie（后端加 `/auth/refresh` 的 Cookie 分支），localStorage 只留非敏感态。

#### FE-P1-4 跳转目标校验不足，`//evil.com` 被放行

- **位置**：`web/src/features/auth/login-form.tsx:41-42`、`web/src/features/auth/register-form.tsx:43-44`
- **现象**：`startsWith('/')` 放行 `//evil.com`。
- **证据**：

  ```tsx
  if (redirect?.startsWith('/')) { router.history.push(redirect) }
  ```

  `redirect` 直接来自可被构造的查询串（`/login?redirect=//evil.com`）。核对 `@tanstack/history` 实现（`pushState`，`@tanstack+history@1.162.1/.../index.js:53,184`），jsdom 实测 `history.pushState({}, '', '//evil.com')` 抛 `SecurityError: cannot update history to the URL http://evil.com/`。

- **影响**：在当前路由版本下不会真的跳到外站，但登录成功后抛异常（RHF `handleSubmit` 会重新抛出，`login-form` 的 onSubmit 无 catch）→ **用户已登录却卡在 /login**，控制台报错。这是典型开放重定向模式，一旦改成 `location.assign` 或路由实现变化就是真实漏洞。
- **修复方向**：白名单校验 `redirect?.startsWith('/') && !redirect.startsWith('//') && !redirect.includes('\\')`，并用 `new URL(redirect, location.origin).origin === location.origin` 双重确认。

#### FE-P1-5 生成的 API 类型未入库，`pnpm typecheck` 不能独立通过

- **位置**：`web/.gitignore:27`、`web/package.json:11`（对比 `:9`）
- **现象**：生成的 API 类型未入库，`pnpm typecheck` 不跑生成，全新检出必失败（实测 18 个错误）。
- **证据**：

  ```text
  .gitignore:27  src/api/types.ts
  package.json:9  "build": "pnpm gen && tsc -b && vite build"
  package.json:11 "typecheck": "tsc -b --noEmit"      ← 没有 gen
  ```

  实测：把 `src/api/types.ts`、`src/routeTree.gen.ts` 移走后 `tsc -b --noEmit` 报 18 条错误（`src/api/client.ts(2,28): error TS2307: Cannot find module './types'`、多处 `TS7006 Parameter 'x' implicitly has an 'any' type`）；只跑 `openapi-typescript`（78.7ms）+ `tsr generate` 后回到 0 错误。

- **影响**：`pnpm typecheck` 不能作为独立 CI 门禁；`types.ts` 被忽略 → **契约漂移在 Git 层完全不可观测**（改了 yaml 忘记重新生成，没有任何检查会发现）。
- **修复方向**：把 `types.ts` 纳入版本控制并在 CI 跑 `pnpm gen && git diff --exit-code`；`typecheck` 加前置 `pnpm gen`。

#### FE-P1-6 同一个 `['my-products']` key 承载两种过滤条件的数据集

- **位置**：`web/src/features/product/commerce-manager.tsx:22`、`web/src/features/product/video-promotion-manager.tsx:15`
- **现象**：两个页面用同一 query key 缓存不同数据集。
- **证据**：

  ```tsx
  queryKey: ['my-products'], queryFn: ... { limit: 100 }                        // 我的商品（全部非删除）
  queryKey: ['my-products'], queryFn: ... { limit: 100, status: 'ON_SALE' }     // 挂商品（仅在售）
  ```

- **影响**：`staleTime = 5s`（`main.tsx:25`）内两个页面复用同一份缓存 → `/video` 页的"挂商品"下拉可能出现已下架商品（用户选中后后端 404/409，报错 toast），或 `/me` 的商品管理看不到 OFF_SALE 商品（无法给它加规格/查库存）。属于"看起来正常、偶发报错"的难查缺陷。
- **修复方向**：key 里带上全部查询变量（`['my-products', { status }]`）。

**P2**

#### FE-P2-1 删除视频后不失效 `['video', id]`

- **位置**：`web/src/routes/video.$id.tsx:52-65`
- **现象**：只失效了 feed / my-videos / user-videos，用户停留在已删除视频详情页：标题、状态、删除按钮原样保留，但 MinIO 对象已删（播放器必然失败）。
- **证据**：`onSuccess` 中无 `['video', id]` 失效调用。
- **影响**：详情页与后端状态不一致，播放必失败。
- **修复方向**：`onSuccess` 里 `queryClient.invalidateQueries({queryKey:['video', id]})` 或 `navigate({to:'/me'})`。

#### FE-P2-2 下单+支付成功后不失效 `['orders']` / `['author-sales']`

- **位置**：`web/src/routes/product.$id.tsx:75-78`
- **现象**：5s 内点"查看订单"看不到新订单。
- **证据**：`onSuccess` 只处理本地跳转/提示，无相关 `invalidateQueries`。
- **影响**：用户立刻查看订单看不到刚下的单，误判下单失败。
- **修复方向**：`onSuccess` 里失效 `['orders']`、`['author-sales']`。

#### FE-P2-3 关注后不失效 `['following-feed']`

- **位置**：`web/src/features/interaction/follow-button.tsx:52-55`
- **现象**：立刻切到"关注"tab 仍是旧列表（staleTime 5s 内不刷新）。
- **证据**：`onSuccess` 只更新 profile 缓存。
- **影响**：关注后内容不变化，用户怀疑关注未生效。
- **修复方向**：`onSuccess` 里失效 `['following-feed']`。

#### FE-P2-4 QueryClient 未配置 retry，4xx 也重试 3 次

- **位置**：`web/src/main.tsx:22-29`
- **现象**：默认对 4xx（404/409/401）也重试 3 次指数退避。
- **证据**：`QueryClient` 构造未传 `retry` 策略。
- **影响**：详情页 404、下单 409 库存不足都会白等几秒并放大后端请求量。
- **修复方向**：按状态码策略重试（`ApiError` 携带 status），4xx 不重试。

#### FE-P2-5 `useEffect` 依赖内联回调，IntersectionObserver 每次渲染重建

- **位置**：`web/src/features/feed/use-sentinel.ts:21`、`index.tsx:160-164`、`user-works.tsx:32-34`
- **现象**：`observe` 会立刻回调一次，配合 `hasNextPage && !isFetchingNextPage` 才没打爆请求。
- **证据**：`useEffect` 依赖数组含内联函数。
- **影响**：无谓的 observer 重建；一旦守卫条件变化就会重复触发翻页。
- **修复方向**：`useRef` 存回调或 `useCallback`。

#### FE-P2-6 blob URL 在渲染期创建且从不回收

- **位置**：`web/src/features/upload/cover-picker.tsx:16`
- **现象**：`URL.createObjectURL(cover)` 在渲染期创建且从不 `revokeObjectURL`。
- **证据**：无 `useEffect` 清理。
- **影响**：每次重渲染泄漏一个 blob URL。
- **修复方向**：`useMemo` + `useEffect` 清理。

#### FE-P2-7 删除按钮 pending 期间未禁用

- **位置**：`web/src/routes/video.$id.tsx:121`
- **现象**：只有 142 行一行"删除中…"文案，按钮仍可点。
- **证据**：无 `disabled={remove.isPending}`。
- **影响**：可重复提交删除请求。
- **修复方向**：加 `disabled={remove.isPending}`。

#### FE-P2-8 未知订单状态渲染 `undefined`

- **位置**：`web/src/routes/orders.tsx:87`
- **现象**：`{labels[order.status]}` 对未知状态渲染 `undefined`（与 CT-7 联动）。
- **证据**：`orders.tsx:20-23` 的 `labels` 是 `Record<Order['status'], string>`，新增状态不会报类型错。
- **影响**：后端一旦新增状态，订单列表状态列空白、状态分支全部不命中。
- **修复方向**：`labels[order.status] ?? order.status`。

#### FE-P2-9 用 `window.location.assign` 跳转登录页

- **位置**：`web/src/routes/index.tsx:62`
- **现象**：整页刷新、丢失 SPA 状态，与同文件其它跳转方式不一致。
- **证据**：同文件其它跳转使用 router navigate。
- **影响**：体验退化、重复请求。
- **修复方向**：改 `navigate({to:'/login'})`。

#### FE-P2-10 静默吞错清单缺乏可观测性

- **位置**：`use-play-report.ts:24-26`、`header.tsx:37-39`、`feed-card.tsx:57/71/78`、`video-player.tsx:17`、`auth-lifecycle.tsx:21`
- **现象**：多数有注释说明，但失败完全无痕。
- **证据**：`use-play-report.ts:24-26` 注释写"计数缺失可接受"，无任何上报。
- **影响**：线上问题无法定位，也无法判断影响面。
- **修复方向**：至少 `console.debug` 或接入前端埋点。

#### FE-P2-11 `['feed']` key 固定但 `initialPageParam` 来自 URL cursor

- **位置**：`web/src/features/feed/use-feed.ts:18-19`
- **现象**：已有缓存时打开分享链接会忽略 cursor（注释承认这是防死循环的取舍）。
- **证据**：`queryKey: ['feed']` 不包含种子 cursor。
- **影响**：分享链接在某些情况下不生效。
- **修复方向**：把种子 cursor 并入 key 或走单独 key。

### 3.3 契约（CT-*）

审计未对契约条目单独分级；下表给出计划阶段的路由（见 [remediation-plan.md](remediation-plan.md)）。

| 编号 | 问题 | 计划阶段 |
|---|---|---|
| CT-1 | route-sync 测试只比较 `method + path`，且有假失败 | B |
| CT-2 | 6 个公开端点被契约声明为需要鉴权 | A |
| CT-3 | 商品列表 limit 文档上限 100，实际 50 且静默截断 | B |
| CT-4 | 限流默认开启，但契约无任何 429、无 `Retry-After` | A |
| CT-5 | 49 个 operation 全部没有 `operationId` | B |
| CT-6 | 契约自称的同步测试文件不存在 | A |
| CT-7 | 退款功能与契约/前端存在同步缺口（进行中） | C |

#### CT-1 route-sync 只比较 `method + path`，schema 漂移不可见且有假失败

- **位置**：`cmd/api/main_test.go:93-124`
- **现象**：route-sync 测试只比较 `method + path`，不比较 schema、参数、响应码、security。
- **证据**：

  ```go
  routeKeys[method+" "+hertzParamRe.ReplaceAllString(r.Path, "{$1}")] = true
  ...
  docKeys[strings.ToUpper(method)+" "+path] = true
  ...
  require.Empty(t, missing, "路由表存在但 openapi.yaml 缺失（改端点必须同步改文档）")
  ```

  实测证据（受控变异，已还原）：把 `OrderPage.items` 从 `array<$ref OrderDetail>` 改成 `type: string` 并新增 `required: [items, totally_made_up]` → `TestOpenAPISync` 仍然 **ok**（漂移漏检）；给 `/api/v1/feed` 加一个合法的 path 级 `parameters:` 块 → 测试 **FAIL**，报 `Should be empty, but was [PARAMETERS /api/v1/feed]`（假失败）。

- **影响**：契约漂移（字段改名、类型变化、状态码变化、security 变化）在 CI 中完全不可见，团队会高估契约保障强度；同时拒绝合法的 spec 写法。
- **修复方向**：① 遍历 `ops` 时只认 `get/post/put/patch/delete/head/options` 白名单（消除假失败）；② 增加 `kin-openapi` 载入 + 用生成的 request/response schema 做结构测试，或引入 spec-first 校验。

#### CT-2 6 个公开端点被契约声明为"需要鉴权"

- **位置**：`internal/api/openapi.yaml:11-12`（全局 `security: [bearerAuth]`）
- **现象**：以下 operation 未用 `security: []` 覆盖：`/health/live`（37-38）、`/health/ready`（48-49）、`/api/v1/auth/register`（72-73）、`/api/v1/auth/login`（93-94）、`/api/v1/auth/refresh`（112-113）、`/api/v1/auth/logout`（133-134）。对照：只有 4 处显式声明匿名（`openapi.yaml:63` docs、`:153` feed、`:248` users/{id}/videos、`:585` products/search）。
- **证据**：

  ```yaml
  # internal/api/openapi.yaml:11-12
  security:
    - bearerAuth: []
  ```

  代码事实：这些端点没有 auth 中间件（`cmd/api/main.go:328` `health.NewHandler(r.db).Register(h)`；`main.go:339` `r.user.RegisterRoutes(g, authMW(r)...)` 中 `authMW` 只挂限流中间件，见 `main.go:317-325`）。

- **影响**：任何按契约生成的 SDK/网关/文档都会给注册、登录、探针错误地加上 Bearer 要求；部分 OpenAPI 校验器/网关可能直接拒绝匿名调用。
- **修复方向**：这 6 个 operation 补 `security: []`。

#### CT-3 商品列表 limit 的契约上限（100）与实际（50）不符

- **位置**：`internal/api/openapi.yaml:1128-1135`、`internal/product/service.go:21,341-347`
- **现象**：文档 `maximum: 100`（描述"合法范围 1-100"），实际 `maxListLimit = 50` 且静默截断。
- **证据**：

  ```go
  maxListLimit = 50
  ...
  if limit > maxListLimit { return maxListLimit }
  ```

  前端确实按文档传 100：`commerce-manager.tsx:24`（`{ limit: 100 }`）、`video-promotion-manager.tsx:17`（`{ limit: 100, status: 'ON_SALE' }`）。

- **影响**：商品数 >50 的商家在"我的商品"里看不到后面的商品（且前端不翻页，直接丢弃 `has_more/next_before_id`）；契约的 100 是错的且没有任何 400 提示。
- **修复方向**：给商品列表单独定义参数（`maximum: 50`，或超限返回 `400 INVALID_ARGUMENT`），前端改用 `before_id` 翻页。

#### CT-4 限流默认开启，但契约里没有任何 429

- **位置**：`internal/api/openapi.yaml`（全文 0 处 `"429"`）；`internal/config/config.go:37`、`.env.production.example`、`internal/middleware/ratelimit.go:83-84`
- **现象**：限流默认开启（`rateLimitOn = true`、`RATE_LIMIT_ENABLED=true`），契约未声明 429，响应也没有 `Retry-After`。
- **证据**：

  ```go
  if !allowed { response.Abort(ctx, response.ErrRateLimited); return }
  ```

  `ratelimit.go` 只 `AbortWithStatusJSON`，见 `response.go:98-109`。

- **影响**：客户端无法从契约得知限流语义；`Error.code` 枚举里有 `RATE_LIMITED` 却没有任何 operation 声明 429；前端因此完全没有 429 分支，只能弹通用错误。
- **修复方向**：公共组件/按分组补 429 响应（含 `Retry-After`），前端对 429 做退避提示。

#### CT-5 49 个 operation 全部没有 `operationId`

- **位置**：`internal/api/openapi.yaml`（grep `operationId` = 0）
- **现象**：全部 operation 缺少 `operationId`。
- **证据**：全文 0 处 `operationId`。
- **影响**：SDK 生成器、运营平台、部分 lint 规则（spectral `operation-operationId`）无法按 id 引用接口；`openapi-typescript` 不受影响所以本地无感。
- **修复方向**：补 `operationId`（可用 `tag + 方法 + 路径` 命名），并在 CI 里加 lint 门禁。

#### CT-6 契约自称的同步测试在仓库中不存在

- **位置**：`internal/api/openapi.yaml:5`
- **现象**：描述写"本文件是 API 契约的唯一事实源，由 openapi_sync_test 保证与代码同步"，但仓库中不存在该测试。
- **证据**：`internal/api/` 只有 `openapi.yaml` + `spec.go`（仅 6 行 `//go:embed`），`go test ./internal/api/...` 输出 `[no test files]`；真实测试是 `cmd/api/main_test.go:76 TestOpenAPISync`，且只做 path 级同步（见 CT-1）。
- **影响**：读者/新人会误以为有 schema 级保障。
- **修复方向**：改描述，或补上真正的同步测试。

#### CT-7 退款功能与契约/前端存在同步缺口（进行中）

- **位置**：`internal/order/model.go:18-20`、`internal/api/openapi.yaml`（全文 0 处 refund）、`cmd/api/main.go`（尚未注册退款路由）、`web/src/routes/orders.tsx:20-23,87`
- **现象**：`model.go` 已新增 `REFUND_REQUESTED`/`REFUNDED`（审计期间的工作区实时改动），契约与前端尚未同步。
- **证据**：

  ```tsx
  // web/src/routes/orders.tsx:20-23 依赖生成的联合类型
  labels: Record<Order['status'], string>
  ```

- **影响**：① 退款路由一旦注册进 `registerRoutes` 而 yaml 未同步，`TestOpenAPISync` 会失败（这是它该做的事，但必须在同一提交里更新契约）；② 前端 `{labels[order.status]}`（`orders.tsx:87`）会渲染 `undefined`，且 `order.status === 'PAID'` 之类的分支全部不命中（无任何操作按钮）。
- **修复方向**：契约与后端同提交更新；前端状态映射加兜底（`labels[order.status] ?? order.status`）。

### 3.4 前端 e2e 覆盖与稳定性（E2E-*）

现状：5 个 spec / 14 个用例（`auth.spec.ts` 3、`commerce.spec.ts` 1、`feed.spec.ts` 4、`social.spec.ts` 4、`upload.spec.ts` 2）。真正有断言价值的是 `commerce.spec.ts` 全链路用例、`social.spec.ts:31-40,61-68` 用 `page.evaluate` 带 token 回查服务端状态、`upload.spec.ts` 真实走 MinIO 预签名 PUT。以下是缺口。

#### 覆盖缺口

| 编号 | 位置 | 现象与证据 | 影响 | 修复方向 |
|---|---|---|---|---|
| E2E-1 | `web/src/api/client.ts:25-55` | 401 静默刷新 + 重试（singleflight、body 缓存重放、auth 端点跳过）零覆盖，需伪造 access token 过期才可测 | 最复杂的客户端逻辑无回归保护 | 加"过期 token → 自动刷新 → 重放原请求"用例 |
| E2E-2 | 后端无端点（`grep refund internal/api/openapi.yaml` = 0） | 退款/售后不是"测试没写"而是"能力未上线" | 主链在"支付/履约"后中断 | 由阶段 C 补齐端点与用例 |
| E2E-3 | `internal/order/expiry_worker.go` | 取消订单 / 超时关单 + 库存回补未覆盖，`EXPIRED` 状态从未被断言 | 关单与回补回归只能靠代码审查 | 造超时订单 + 断言库存回补与流水 |
| E2E-4 | `internal/order/service.go` | 409 库存不足下单失败、幂等重放（同 `idempotency_key` 重复下单/重复支付）未覆盖 | 幂等承诺无端到端证据 | 补 409 与重复请求返回同一订单的用例 |
| E2E-5 | `GET /users/{id}/following`、`/followers` | 前端根本没有消费这两个端点（无社交列表页），既无 UI 也无 e2e | 已实现能力无人使用 | 明确列为非目标并记录，或在阶段 B 做最小接入 |
| E2E-6 | `GET /products/search` | 无 UI、无 e2e（商品发现只有 Feed 卡片入口） | 同上 | 同上 |
| E2E-7 | `GET /products/{id}/skus/{sku_id}/stock`、`POST .../skus/{sku_id}/status`、`PATCH /videos/{id}/products/{product_id}` | 三个已文档化、已实现的端点前端未接入 | 商家缺少库存查询/停用规格/排序入口 | 阶段 B 最小接入并补 e2e |
| E2E-8 | `internal/middleware/ratelimit.go`、`internal/feed/cursor.go` | 限流 429 行为、`INVALID_CURSOR` 400（游标篡改/过期）未覆盖 | 错误语义无验证 | 补 429 与篡改游标用例 |
| E2E-9 | `header.tsx:20` 等 | 错误态/空态未覆盖：视频 404、用户不存在、后端离线、上传失败重试分支 | 故障时用户看到什么无证据 | 补错误/空态用例 |
| E2E-10 | `web/src/lib/auth-lifecycle.tsx` | 多标签页 refresh 轮换互踢无回归测试 | FE-P1-3 修完无保护 | 双 context 用例断言互踢消失 |

#### 稳定性与顺序依赖

| 编号 | 位置 | 现象与证据 | 影响 | 修复方向 |
|---|---|---|---|---|
| E2E-F1 | `web/e2e/feed.spec.ts:31-34` | 唯一的硬 sleep：`for (let i = 0; i < 14; i++) { await page.mouse.wheel(0, 860); await page.waitForTimeout(150) }` | 机器慢/CI 冷启动时不足以触发翻页 → 断言 scrollHeight 增长失败 | 改用 `expect.poll` 包住滚动推进 |
| E2E-F2 | `web/e2e/feed.spec.ts:4-10`、`web/e2e/social.spec.ts:53-54` | 依赖库中已有已发布视频（无 seed、无 beforeAll 造数）；空库单跑 `feed.spec.ts` 必挂，全集能过只因文件名字母序 `commerce < feed`（`playwright.config.ts:11-12 fullyParallel:false, workers:1`） | **隐式顺序依赖**，改文件名/并行度即崩 | 用 API 造数 fixture，去掉对前序 spec 副作用的依赖 |
| E2E-F3 | `web/e2e/social.spec.ts:22-28` | 用 `Number(textContent)` 解析被格式化过的点赞数；`formatCount`（`format.ts:10-15`）在 ≥1000 输出 `1.0k` → `Number('1.0k') = NaN` | 共享库点赞数过千后用例自己坏掉（非产品 bug） | 改断言 `aria-pressed` 或服务端值 |
| E2E-F4 | `web/playwright.config.ts:18` 与 `:21` | 顶层 `viewport: {width:390,height:844}` 被 project 的 `devices['Desktop Chrome']` 覆盖（`mergeObjects(config.use, projectConfig.use, ...)` 顺序，设备描述符 `viewport: {width:1280,height:720}`）；feed 用例里 `mouse.move(195,500)`/`wheel(0,860)` 是按 390×844 手调的常量 | 既没测到移动布局，又让滚动步长与"整屏"假设不符 | 把 viewport 放进 project 的 `use`（或改用 `devices['Pixel 5']`） |
| E2E-F5 | `web/playwright.config.ts:22-25` | `command: 'pnpm dev'` + `reuseExistingServer: true` + 固定 5173，且无 `retries`（默认 0） | 复用开发机上可能正在跑的旧 dev server，CI 与本地行为不一致 | CI 用独立 build + preview，明确 `retries` |
| E2E-F6 | `commerce.spec.ts` 两处、`social.spec.ts` 三处 | 用例直接绑定 zustand persist 结构：`JSON.parse(localStorage.getItem('tokmall-auth') ?? '{}').state?.accessToken` | store 一改名/换 storage 全部静默变 `undefined` | 抽出 helper 并加存在性断言 |
| E2E-F7 | `web/e2e/utils.ts:4-13` | 无数据清理：每个用例注册新用户 + 发布新视频，Feed 单调增长 | feed.spec 的窗口化/游标断言随时间变脆 | 独立库 + 每次运行前 migrate/清理 |
| E2E-F8 | `web/e2e/utils.ts:25-38` | `expectPlaying` 导出但无人调用 | 死代码 | 删除或接入 |

## 4. 能力缺口分析

以下不是 bug，而是"面试官会问、项目现在答不上来"的空白。每条给出被问的方式与缺失的代价。

### G-1 售后/退款闭环缺失

- **会被怎么问**："用户付了钱要退款怎么办？""退款和发货同时发生会怎样？""退款金额谁决定？"
- **现状**：`internal/api/openapi.yaml` 全文 0 处 refund；订单状态机到 `COMPLETED` 即终止；审计期间工作区已有并发的退款开发（`refund_*.go`、`000015_order_refund.*`），但契约、前端、测试都还没跟上（CT-7）。
- **代价**：业务闭环在"支付/履约"后中断，无法回答"钱怎么退回去"；`commerce-evolution-plan.md` C1 与 `README` 都已声明"退款/售后未实现"，面试时这是一条明写的缺口而不是可讲的设计。

### G-2 支付后没有可靠事件分发（无 Outbox / 无 MQ）

- **会被怎么问**："支付成功之后要发通知、更新统计、给风控送数据，这些怎么做才不丢不重？""如果消息队列挂了，支付还能成功吗？"
- **现状**：支付事务只写 `orders`/`payments`；没有任何 `outbox_events`、没有 relay、没有消费者。`interview-production-readiness-plan.md` 阶段 4 与 `commerce-evolution-plan.md` C3 都还是"未开始"；`M5-Kafka`（播放事件异步化）已因"压测证明同步写不是读路径瓶颈"而搁置——这是另一个决策，不能拿来回答支付侧的问题。
- **代价**：所有"支付后副作用"只能同步塞进支付请求里，或干脆不做；没有幂等消费、DLQ、重放的概念，可靠事件是面试中最常被追问的分布式基本功之一。

### G-3 没有对账

- **会被怎么问**："你怎么证明库存账实一致？""订单、支付、库存流水、作者 GMV 四个数对不上时你怎么发现？"
- **现状**：不超卖有 500 并发/100 库存的正确性实验（`interview-notes/order-concurrency-proof.md`），但没有可重复执行的只读对账报告；`commerce-evolution-plan.md` C0 已把它列为计划项。
- **代价**："正确"目前靠单次实验与约束推断，而不是靠持续可验证的数据一致性证据；一旦出现差异（例如 BE-P2-3 的 ticket 烧毁、BE-P2-4 的缓存死链、BE-P2-12 的多写入口漂移），没有任何机制能发现。

### G-4 没有业务指标

- **会被怎么问**："你怎么知道线上订单成功率是多少？""库存不足、幂等重放、状态冲突、关单积压这些有多少？"
- **现状**：`/metrics`（9091）只有 Hertz 路由级指标 + `observability/dbpool.go`、`observability/feedcache.go` 两个 collector；商域没有任何业务计数器。
- **代价**：所有交易结论只能靠测试推断，无法回答"量级多大、趋势如何、异常怎么发现"；也无法给故障演练提供观测面（见 G-7）。

### G-5 没有 CI

- **会被怎么问**："你的门禁是什么？""合代码之前跑什么？"
- **现状**：仓库没有 `.github/`（无 workflow）；`pnpm typecheck` 不跑生成（FE-P1-5），route-sync 只比路径（CT-1），契约漂移在 Git 层不可观测。
- **代价**：目前"绿灯"依赖开发者本机按正确顺序执行命令；审计已证明全新检出直接 `pnpm typecheck` 会报 18 个错误。面试官只要问"我 clone 下来跑什么"，就会暴露工程化缺口。

### G-6 下单链路没有压测

- **会被怎么问**："你这个下单接口能扛多少？""瓶颈在哪？""热点 SKU 会怎样？"
- **现状**：`docs/performance-baseline.md` 是 Feed 读路径（1M 数据 6.6k→11.3k QPS）与连接池基线；下单/支付/关单链路只有正确性实验（并发 500 恰好 100 成功），没有吞吐、P95/P99、行锁等待与池等待数据；`commerce-evolution-plan.md` C0 正是为此而设。
- **代价**：所有性能叙事都停留在 Feed；"为什么不引入 Redis 预扣/秒杀"这类问题没有数据支撑，只能回答"还没测"。

### G-7 没有故障演练

- **会被怎么问**："Redis 挂了会怎样？""关单 Worker 重启时会重复回补吗？""PG 抖动时用户看到什么？"
- **现状**：代码层面有正确的失败语义（限流 fail-open、迁移/约束兜底、`FOR UPDATE` + 条件更新），但没有演练记录。
- **代价**：失败语义只被单元测试与代码审查覆盖，从未在真实依赖故障下验证过；面试中"你怎么知道它真的这样表现"无法回答。`commerce-evolution-plan.md` C2 与 `interview-production-readiness-plan.md` 阶段 5 都已列入计划。

## 5. 已确认做对的部分

以下为两份审计核对通过、可直接用于面试正面陈述的事实：

- **无 SQL 注入**：全部 `Raw/Exec` 均为参数绑定；唯一字符串拼接是 `internal/attribution/repository.go:29-37` 的常量片段，无用户输入进入 SQL 文本。
- **无 IDOR/越权**：写端点都从 JWT 取 actor 并在 WHERE 中限定（`orders.user_id`、`merchant_id`、`videos.author_id`、`skus→products.merchant_id` 归属链）；`attribution` 显式 `authorID != caller → 404`（`internal/attribution/handler.go:30-33`）；avatar/cover 需核销本人 ticket，无法引用他人对象 key。
- **认证边界清晰**：所有变更型路由只注册在 protected 组（`cmd/api/main.go:340-360`），optional 组只有 GET；`cmd/api/main_test.go:76` 的 route-sync 保证路由与 OpenAPI 双向一致。
- **JWT 校验严格**：`WithValidMethods(["HS256"])` + HMAC 类型断言 + `WithExpirationRequired` + access/refresh 类型区分（`internal/token/token.go:48-73`）；refresh 黑名单按 jti + 剩余 TTL，`Logout` 幂等。
- **密码存储规范**：bcrypt + 长度 8..72 上限 + 未知账号走 dummy hash 做时间对齐（`internal/user/service.go:209-213,256,266`）；日志不含口令/token。
- **五类幂等**：likes/follows（PK + `ON CONFLICT DO NOTHING`）、play_events（`event_id` PK + 业务指纹比对，冲突且载荷不同 → 409）、inventory（`(sku_id,reason,biz_id)` 唯一 + 载荷比对）、订单（`(user_id,idempotency_key)` 唯一 + SHA256 指纹）、支付（`payment_no`/`provider_trade_no` 双唯一 + `sameSuccessfulPayment`）。
- **库存非负/不超卖**：`order.Create` 用 `quantity >= ?`、`inventory.Adjust` 用 `quantity + ? >= 0`，DB 侧 `migrations/000011:43-45` 有 `CHECK (quantity >= 0)`；并发建单依赖行锁 + EvalPlanQual 重新求值，无丢更新。
- **无重复回补/重复退款路径**：取消与关单均为 `FOR UPDATE` 锁订单行 + `status='PENDING_PAYMENT'` 条件更新 + `RowsAffected==1` 校验，每单最多回补一次；`Pay` 不改库存且 `PAID→CANCELED` 非法；集成测试断言"支付与过期只能一个赢"（`internal/order/postgres_integration_test.go:317-357`）。
- **状态机双层防护**：video/order/product/SKU 均表驱动 + `WHERE status=from` 条件更新；订单状态与时间戳由 `migrations/000012:78-82`、`000014:5-8` CHECK 兜底；`video_products` 5 条上限由 advisory xact lock 触发器保证（`000013`）。
- **游标分页正确**：三套游标均 HMAC 签名 + `rank_version` 校验 + 键集（非 offset），篡改/版本不符 → 400；跨端点复用被 `rank_version` 阻断。
- **共享状态处理克制**：仅一个后台 goroutine（订单过期 Worker），无可变共享 map；缓存/singleflight 返回值一律 `cloneFeedResult` 后再写请求级字段（`internal/feed/service.go:370-384`）。
- **panic/资源管理**：recovery 在链路最外层（`cmd/api/main.go:108-115`），业务代码无 `panic`；Redis/SQL/worker 在启动关闭路径均正确释放；`context.Background()` 仅出现在启动/关闭路径。
- **错误处理链一致**：repo 翻译哨兵 → service 分类 → handler 映射 BizError，未知一律 500；哨兵一律 `errors.Is`；`response.statusByCode` 覆盖全部 BizError 码。
- **迁移纪律**：无 AutoMigrate；版本化 SQL 双向配对；代码依赖的唯一约束/部分唯一索引均存在（`orders_user_idempotency_key`、`payments_payment_no_key`、`idx_payments_provider_trade_no_unique`、`inventory_transactions_sku_reason_biz_id_key`、`order_items_single_item_per_order_key`、`video_products_video_product_key`）；已核对 gorm 源码确认 `ON CONFLICT DO NOTHING` + `RETURNING` 下 `RowsAffected` 与自增 ID 语义正确。
- **前端工程质量**：手写代码零 `any`/零类型逃逸（仅生成的 `routeTree.gen.ts` 含 `as any`）；15 处 `.map()` 全部有稳定 key；Feed 商品卡由后端批量装配（`internal/feed/product_cards.go:50` `WHERE vp.video_id IN ?`），无 N+1；乐观更新方向走 mutation variables，无闭包陷阱；路由守卫与根级错误/404 兜底齐备；上传链路与契约严格对齐（含 PUT 显式 `Content-Type`，注释记录了 ORB 拦截事故）；登出会清空整个 QueryClient。
- **契约基本盘**：40 个 path / 49 个 operation 与 `Register*Routes` 双向同步（实测 PASS）；55 个 schema、11 个 parameter、11 个 response 的 `$ref` 无断链；`Error.code` 的 23 个枚举值与 `internal/response/response.go:15-37` 逐字段一致；商域五个模块的请求/响应 schema 齐备且与 Go DTO 对齐。
- **e2e 有真实价值的部分**：`commerce.spec.ts` 单用例覆盖"创建商品 → 加规格 → 补库存 → 上传 → 发布 → 挂商品 → 下单+模拟支付 → 订单查询 → 发货 → 确认收货 → GMV 断言 → 下架 → 不可购买"，是全仓库最有价值的一个用例；`social.spec.ts` 用 `page.evaluate` 回查服务端 `is_liked`/`is_following`，不是只看 UI。

## 6. 遗留与不确定项

审计中标注"需确认/需验证"、本文未替其下结论的事项：

1. BE-P2-7 的 PG 错误码 22003 需在真实 PG 上复核。
2. BE-P2-11 的 OFF_SALE 可见性是否刻意，需产品语义确认。
3. BE-P2-1 的 403/404 取舍需明确（`service_test.go:477` 与 OpenAPI 已固化 403）。
4. `internal/order/refund_*.go` 与 `migrations/000015_order_refund.*` 是审计期间的并发工作，未纳入本次审计结论；其正确性需在阶段 C 单独验证。
