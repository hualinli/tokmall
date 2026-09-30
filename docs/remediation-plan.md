# TokMall 修复与演进计划（2026-10）

> 输入：[项目审核报告（2026-10）](project-audit-2026-10.md) 的稳定编号（`BE-*` / `FE-*` / `CT-*` / `E2E-*`）。
> 基线：`560dda1`（2026-10-01）及其工作区状态。本计划不改变审计编号；条目是否完成以本文列出的验收证据为准，不以"代码看起来改了"为准。
> 原则：先收口已有能力的安全与正确性，再补业务闭环，最后补可证明性；不为"架构数量"引入 Kafka/微服务；每条简化决策写进 ADR。

## 1. 总体目标与验收标准

### 1.1 目标形态

```text
业务闭环：内容 → 商品 → 下单 → 支付 → 履约 → 售后退款 → 归因 → 对账
工程闭环：契约 → CI → 指标 → 演练 → 文档
```

两条闭环都要"可运行、可验证、可讲述"：任何一个结论都能回指代码、测试或实验数据。

### 1.2 业务闭环验收标准

1. 一条 Playwright 主链路可反复跑通：注册 → 上传并发布视频 → 建商品/SKU/补库存 → 视频挂商品 → Feed 进入 → 下单 → 支付 → 商家发货 → 买家确认完成 → 申请退款 → 商家裁决 → 退款执行 → 订单/库存/归因三处状态一致。
2. 真实 PostgreSQL 并发测试证明：同一退款申请重复提交只退一次；"退款 vs 发货/确认完成"竞争只产生合法状态；退款触发的库存回补最多一次；非买卖双方操作返回 404。
3. 归因口径固定并可对账：明确 GMV 是总额还是净额，退款后如何冲减，且 `orders`、库存流水、作者统计三者使用同一规则。

### 1.3 工程闭环验收标准

1. **门禁**：`.github/workflows/ci.yml` 在 main 上必过，至少包含：`go test ./...`、关键包 `-race`、隔离 PG 集成测试（`TOKMALL_RUN_PG_INTEGRATION=1`）、迁移 up/down 验证、`TestOpenAPISync`、OpenAPI lint（0 error）、`pnpm gen && pnpm typecheck && pnpm lint && pnpm test && pnpm build`、契约漂移检查、e2e。
2. **契约**：`types.ts` 入库且 `pnpm gen:api && git diff --exit-code` 通过；除 route-sync 外有 schema 级同步测试，能抓出 CT-1 的变异实验（`OrderPage.items` 改类型必须 FAIL），且对合法的 path 级 `parameters` 不再假失败。
3. **指标**：`/metrics` 可查订单成功率、库存不足、幂等重放、状态冲突、关单积压、退款失败/重试、对账差异、Outbox backlog/消费延迟/DLQ；README 说明每个指标的含义与告警阈值来源。
4. **对账**：一条命令产出只读对账报告（库存余额 vs 流水、已支付订单 vs 支付事实、取消/过期回补次数、作者 GMV vs 订单事实、退款 vs 资金流水、Outbox 状态分布），正常数据下差异为 0；有差异时的定位与修复步骤写入文档。
5. **压测**：下单链路在固定数据规模/并发/连接池/机器下有成功率与 P95/P99 基线，含热点 SKU 与散列 SKU 两组；结论写入 `performance-baseline.md`。
6. **演练**：至少 4 次依赖故障演练（PostgreSQL、Redis、MinIO、MQ/Consumer 或 Worker 重启），每次记录用户可见表现、指标变化、恢复步骤与数据一致性结论。
7. **审计条目收口**：P0、P1 全部关闭；P2 全部关闭或记录为 ADR 化的已知取舍；`docs/` 中不存在"已完成"但无验收证据的功能。

## 1.5 执行进度（2026-10-01 更新）

| 阶段 | 状态 | 提交 | 验收证据 |
|---|---|---|---|
| A 安全与正确性收口 | ✅ 已完成 | `ab8c92a` | BE-P0-1、BE-P1-1..8 全部修复并回归；`go test ./...` 16 包全绿，6 个包 `-race` 通过；真实 PG 集成测试（含新增"晚到支付"用例）连续 3 次通过 |
| B 前端与契约收口 | 🚧 进行中 | — | FE-*、CT-1/3/5 与 e2e 补强（见后续提交） |
| C 售后/退款闭环 | ✅ 已完成 | `6e1dd1e` | `internal/order/refund_postgres_integration_test.go`：并发同意只执行一次、已发货不回补、拒绝后可再申请、两组竞争只有一个终态、越权无副作用；顺带修复"履约命令复用完整状态图"的缺陷 |
| D Outbox + MQ | ✅ 已完成（Kafka 为可替换实现，未接入） | `ff288ac` | `internal/outbox/integration_test.go` 六场景全绿（恰好一次、重复不累加、退款冲减、回滚不留事件、DLQ 重放、崩溃认领）；`scripts/drill_commerce_loop.sh` 端到端跑通 |
| E 对账与可证明性 | ✅ 已完成 | `1436fd5`、`68192ea` | 六项检查 + 对账驱动重放 + 读模型重建；契约 schema 级校验（变异验证）；CI 四 job；压测基线写入 `performance-baseline.md`；故障演练脚本可复现 |
| F 文档与面试交付 | ✅ 已完成 | `73bfb45` | `docs/interview-walkthrough.md`（十个故事 + 问答速查 + 边界清单）、审核报告与修复计划、ADR-014～020、README 与 AGENTS 同步 |

> 阶段 A/E 的执行过程中额外发现并修复了两个**计划外**的真实缺陷：
> 1. 履约命令复用完整状态图，导致"退款中订单可被发货"（被数据库 CHECK 约束拦下，
>    集成测试暴露）——见 `6e1dd1e` 与 `docs/interview-notes/refund-state-machine-and-stock.md`；
> 2. User model 映射 `status` 后 GORM 写入空值，绕过 `DEFAULT 'ACTIVE'`，
>    导致"注册成功但登录 401"——由端到端演练发现，见 `4a824a6`。

## 2. 阶段总览

| 阶段 | 目标 | 覆盖审计条目 | 依赖 | 主要产出 |
|---|---|---|---|---|
| A 安全与正确性收口 | 关掉全部 P0/P1 与选定 P2 | BE-P0-1、BE-P1-1..9、BE-P2-2/3/5/6/7/8/9/10、CT-2/4/6 | 无 | 修复提交 + 回归测试 + 最小迁移 |
| B 前端与契约收口 | 补前端缺陷、契约工程化、e2e 补强 | FE-P0-1、FE-P1-1..6、FE-P2-1..11、BE-P2-1、BE-P2-11、CT-1/3/5、E2E-1..10、E2E-F1..F8 | A（429/错误码语义定型后前端才能建模） | 前端修复 + vitest 单测 + OpenAPI lint + schema 级同步测试 + CI 门禁雏形 |
| C 售后/退款闭环 | 补齐业务闭环缺口 | CT-7、BE-P2-4、BE-P2-12、E2E-2 | A（幂等/事务/错误语义）；B 的状态映射兜底可并行 | 退款状态机/表/接口/前端 + 真实 PG 并发测试 + 主链路 e2e |
| D Transactional Outbox + MQ | 支付后可靠事件分发 | 新增能力（无审计条目） | A；与 C 并行，`OrderRefunded` 事件需 C 定型 | outbox 表 + relay + 幂等消费 + DLQ + 重放 + 抽象 transport（默认 Redis Streams） |
| E 对账与可证明性 | 让结论可自证 | 能力缺口 G-3/4/5/6/7；审计条目的回归证据 | A、B、C、D | CI、业务指标、对账命令、下单压测、故障演练记录 |
| F 文档与面试交付 | 把证据变成可讲述的材料 | 全部条目的收尾记录 | A–E | README、面试讲解主文档、ADR-014..020、面试笔记 |

依赖的关键路径是 **A → C → E**；B 与 D 可以与 C 部分并行，但 D 的消费者语义依赖 A 收口后的错误与幂等约定。

## 3. 阶段详细

### 阶段 A：安全与正确性收口

**范围**

修复后端全部 P0、P1 与"低风险、可被测试锁死、属于安全或数据正确性"的选定 P2，以及三条必须同提交落地的契约修正。

**要修的审计条目**

| 条目 | 位置 | 关键动作 |
|---|---|---|
| BE-P0-1 | `internal/middleware/ratelimit.go:94`、`cmd/api/main.go:99-106` | 只信任配置的代理网段（`WithClientIPFunc`）或改用 `ctx.RemoteAddr()`；可信范围写入部署文档 |
| BE-P1-1 | `ratelimit.go:50`、`user/service.go:253` | 限流 key 与业务查询共用同一份账号规范化（TrimSpace + 小写） |
| BE-P1-2 | `order/repository.go:165` | `Pay` 的锁内条件加 `expires_at > now()`；Worker 支持批量循环 |
| BE-P1-3 | `user/service.go:346` | refresh 旋转改为 `SetNX` 抢占，返回 false 直接 401 |
| BE-P1-4 | `feed/service.go:400`、`:264-270` | singleflight 闭包使用 `context.WithoutCancel` 或独立超时 ctx |
| BE-P1-5 | `video/service.go:511` | `session_id ≤128`、`trace_id ≤64`；22001 翻译为领域哨兵 → 400 |
| BE-P1-6 | `video/handler.go:194` | `trace_id` 只接受服务端生成值或校验格式后截断 |
| BE-P1-7 | `user/service.go:199` | 存 `addr.Address` 规范化值 + 长度 ≤254 + `lower(email)` 唯一索引兜底 |
| BE-P1-8 | `interaction/handler.go:157` | 补齐 `ErrInvalidParam→400`、`ErrUserNotFound→404`，default 才 500 且记日志 |
| BE-P1-9 | `video/service.go:173` | 改 `PresignedPostPolicy`（`content-length-range`）或上传后 `StatObject` 比对 + 每用户配额 |
| BE-P2-2 | `feed/service.go:492` | 迁移补 `CHECK (status <> 'PUBLISHED' OR published_at IS NOT NULL)`；代码侧对 nil 跳过 |
| BE-P2-3 | `video/service.go:236` | ticket 核销与 `CreateVideo` 同一事务 |
| BE-P2-5 | `product/service.go:119` | title ≤200、description ≤2000、bio ≤500、spec ≤200 |
| BE-P2-6 | `product/repository.go:82` | 转义 `\ % _` + 关键字长度上限（≤64） |
| BE-P2-7 | `inventory/service.go:68` | `change` 绝对值上界（≤ 1e9），越界返回 400 |
| BE-P2-8 | `user/service.go:249` | 登录/刷新/鉴权统一加 `status='ACTIVE' AND deleted_at IS NULL`，并与 interaction 可见性对齐 |
| BE-P2-9 | `cmd/api/main.go:101` | `/metrics` 绑定内网地址（配置化），文档标注不可暴露公网 |
| BE-P2-10 | `config/config.go:18` | 所有环境强制显式 `JWT_SECRET`/`FEED_CURSOR_SECRET`，仅测试注入默认值 |
| CT-2 | `openapi.yaml:37,48,72,93,112,133` | 6 个公开 operation 补 `security: []` |
| CT-4 | `ratelimit.go:83-84`、`response.go:98-109`、`openapi.yaml` | 补 429 响应定义与 `Retry-After` 头 |
| CT-6 | `openapi.yaml:5` | 描述改为事实（真正的同步测试由阶段 B 交付） |

**非目标**

- 不引入 MQ/Outbox（阶段 D）；不做退款功能（阶段 C）。
- 不改 403/404 语义与 OFF_SALE 可见性（BE-P2-1、BE-P2-11 在阶段 B 做产品决策后落地）。
- 不重构库存写入路径（BE-P2-12 与退款回补同源，放阶段 C）。
- 不动前端页面；CT-2/CT-4 会改变生成的类型，合并前必须重跑 `pnpm gen`。

**交付物**

- 代码：上表逐条修复，一条一个提交。
- 迁移：`000016` 起的 CHECK 与索引（含 `lower(email)` 唯一索引）；up/down 在真实 PG 上验证。
- 测试：限流伪装回归（伪造 `X-Forwarded-For` 不换桶、空白前缀不换桶）、超时订单支付被拒、并发 refresh 只有一个成功、singleflight leader 取消不扩散、`session_id`/`trace_id` 超长返回 400、邮箱规范化与长度、unfollow 400/404、ticket 核销失败回滚。
- 文档：`docs/backend-architecture.md` 补充可信代理与 `/metrics` 暴露面；`AGENTS.md` 更新限流与配置约定。

**验收证据**

- `go test ./...` 与相关包 `-race` 通过；真实 PG 上迁移 up/down 通过。
- 新增回归测试逐条对应上表条目，且能在修复前复现（保留失败用例为回归）。
- `go test ./cmd/... -run TestOpenAPISync` 通过；`pnpm gen && pnpm typecheck` 通过。

**依赖关系**

无前置。阶段 B 的 429 分支与 D 的错误语义约定都以本阶段的结果为准，因此 A 必须先合并。

**风险与回滚**

- 限流 IP 变更：若部署环境有反向代理但未配置可信网段，可能把整站算作一个 IP → 用配置项控制，保留现有 fail-open 行为；回滚为单个配置值。
- 邮箱唯一索引：存量重复邮箱会让迁移失败 → 先跑查重 SQL 并输出清洗清单，索引与清洗分开提交。
- `Pay` 加 `expires_at` 判断会改变"超时但未关单"订单的行为（支付失败而非成功）→ 返回明确错误码并在文档说明。
- 每条修复独立提交，回滚以单提交 revert 为准；禁止把多条修复混进一个提交。

**提交信息建议**

```text
fix(ratelimit): trust only configured proxies for client IP
fix(auth): normalize account rate-limit key with the service layer
fix(order): reject payment after expires_at inside the lock
fix(user): claim refresh rotation atomically with SetNX
fix(feed): detach singleflight work from the leader request context
fix(video): validate play event session_id and trace_id length
fix(user): normalize email and enforce column width
fix(interaction): map unfollow errors to 400/404
fix(video): bound presigned upload size with a POST policy
fix(config): require explicit JWT secret in every environment
docs(openapi): declare public endpoints and rate limit responses
```

### 阶段 B：前端与契约收口

**范围**

修复全部前端 P0/P1 与 P2，补齐契约与类型的工程化保障，改造 e2e 的覆盖与稳定性；同时落实两条需要产品语义的后端条目。

**要修的审计条目**

| 条目 | 位置 | 关键动作 |
|---|---|---|
| FE-P0-1 | `product.$id.tsx:29` | `crypto.randomUUID` 懒初始化 + 安全上下文 fallback（统一 uuid 工具） |
| FE-P1-1 | `follow-button.tsx:52-54,88-96` | 缓存更新幂等化，增量落到 `followers_count` |
| FE-P1-2 | `like-button.tsx:57-58`、`user-works.tsx:16` | 统一 queryKey 工厂，消除精确匹配不中的静默 no-op |
| FE-P1-3 | `auth-store.ts:38-46`、`auth-lifecycle.tsx:18-26` | `BroadcastChannel`/storage 同步 + 单飞刷新；HttpOnly Cookie 迁移见 ADR-018 |
| FE-P1-4 | `login-form.tsx:41-42`、`register-form.tsx:43-44` | redirect 白名单（拒绝 `//`、`\`）+ origin 二次确认 |
| FE-P1-5 | `web/.gitignore:27`、`web/package.json:11` | `types.ts` 入库；`typecheck` 前置 `pnpm gen`；CI 加契约漂移检查 |
| FE-P1-6 | `commerce-manager.tsx:22`、`video-promotion-manager.tsx:15` | query key 带上全部查询变量 |
| FE-P2-1..11 | 见审核报告 3.2 | 逐条修复（失效遗漏、retry 策略、observer 依赖、blob 回收、pending 禁用、状态兜底、navigate、埋点、cursor key） |
| BE-P2-1 | `video/service.go:315` | 决策：统一 404（见 ADR-020），同步 OpenAPI、`service_test.go:477` 与前端错误分支 |
| BE-P2-11 | `product/service.go:157` | 决策：下架商品对非商家返回 404，商家可见只读视图；同步搜索/详情语义 |
| CT-1 | `cmd/api/main_test.go:93-124` | 方法白名单消除假失败 + `kin-openapi` 载入 + schema 级结构测试 |
| CT-3 | `openapi.yaml:1128-1135` | 商品列表 `maximum: 50`（或超限 400），前端改用 `before_id` 翻页 |
| CT-5 | `openapi.yaml` 全文 | 49 个 operation 补 `operationId` + lint 门禁 |
| E2E-1..10 | 见审核报告 3.4 | 按覆盖缺口逐条补用例（退款一条随阶段 C） |
| E2E-F1..F8 | 见审核报告 3.4 | e2e 稳定性改造：去硬 sleep、去顺序依赖、独立库造数、project 级 viewport |

**非目标**

- 不替换状态管理/UI 框架，不做前端架构重写。
- 不把 refresh token 迁移到 HttpOnly Cookie（ADR-018，触发条件满足时再做）。
- 不为 `GET /users/{id}/following`、`/followers`、`GET /products/search` 新建完整页面（E2E-5、E2E-6 记为已知边界，不做 UI）；只补齐 E2E-7 的三个已实现端点的最小接入。
- 不引入 MSW/组件测试框架；单测只覆盖审计点出的高危纯逻辑。

**交付物**

- 代码：上表修复；`web/src/features/product/commerce-manager.tsx` 等页面按 `before_id` 分页。
- 测试：vitest 单测（`updateProfileCache` 幂等与字段、`updateFeedResultCount` key 匹配、redirect 白名单、`formatCount` 边界）；e2e 稳定性改造；新增 401 刷新重放、409 库存不足、幂等重放、429、`INVALID_CURSOR`、错误/空态、多标签互踢用例。
- 工具链：`tsconfig.e2e.json`（把 `e2e/**`、`playwright.config.ts`、`vitest.config.ts` 纳入 `tsc`）；OpenAPI lint（redocly 或 spectral）接入；契约漂移 job。
- 文档：`web/README.md` 更新运行与生成纪律；`docs/README.md` 记录已知边界。

**验收证据**

- 全新 clone（无 `types.ts`）直接 `pnpm typecheck` 通过；`pnpm gen:api && git diff --exit-code` 通过。
- 复现审计的两个变异实验：`OrderPage.items` 改成 `string` → schema 级测试必须 FAIL；给 `/api/v1/feed` 加 path 级 `parameters` → route-sync 必须 PASS。
- `pnpm test` 有 ≥4 个单测文件且能分别复现 FE-P1-1/2/4 与 `formatCount` 缺陷；`pnpm test:e2e` 在干净库上不依赖文件顺序通过，`--repeat-each=3` 无 flaky。
- `social.spec.ts` 不再解析 `formatCount` 文案；`feed.spec.ts` 不再有固定 `waitForTimeout` 循环。

**依赖关系**

依赖阶段 A：429 与错误码语义定型后前端才能写退避与错误分支；CT-2/CT-4 改动会让生成类型变化，B 的 typecheck 门禁必须在 A 之后接入。

**风险与回滚**

- `types.ts` 入库后每次改 yaml 都会产生 diff → 用"改 yaml 必须同提交 gen"的纪律 + CI 兜底；回滚为把文件移回 gitignore（会重新失去漂移可见性，不建议）。
- e2e 改造需要独立数据库与造数 fixture → 本地开发库与 CI 库分离；回滚为保留旧 spec 并标记 `skip`（仅限过渡期）。
- `BroadcastChannel` 在旧 Safari/隐私模式不可用 → 降级到 storage 事件 + 一次性刷新兜底。
- 403→404 与 OFF_SALE 语义变更会改动既有测试与前端分支 → 同提交同步 OpenAPI、`service_test.go` 与 e2e，避免中间态。

**提交信息建议**

```text
fix(web): lazy-init order idempotency key outside render
fix(web): make follow cache update idempotent and target followers_count
fix(web): align like-button query key with user works grid
fix(web): sync auth token across tabs and single-flight refresh
fix(web): validate post-login redirect against same-origin URLs
build(web): commit generated api types and bootstrap typecheck
test(web): add unit tests for cache updates and redirect guard
test(e2e): remove hard sleeps and implicit spec ordering
feat(api): add schema-level contract test and openapi lint
fix(video): unify delete authorization to 404
```

### 阶段 C：售后/退款闭环

**范围**

补齐"申请 → 裁决 → 退款 → 库存回补 → 归因口径"的完整闭环，并同步契约、前端与测试。

**要修的审计条目**

| 条目 | 关键动作 |
|---|---|
| CT-7 | 退款路由/状态/错误码与 `openapi.yaml` 同提交；前端状态映射兜底（FE-P2-8 已在 B 落地） |
| BE-P2-4 | 关注 Feed 缓存失效与视频状态变更联动（同一 write path 设计，避免退款回补/删除各写一套） |
| BE-P2-12 | 库存写入口收口为单一接口，退款回补走同一路径 |
| E2E-2 | 退款段的主链路 e2e 用例 |

**业务规则（先定规则再写代码）**

```text
PAID / FULFILLING  → 可申请整单退款（是否允许 COMPLETED 后退款：本阶段不允许）
REFUND_REQUESTED   → REFUNDED（商家同意）| 拒绝回到原状态并记录裁决备注
退款金额 = 订单总额（从订单快照派生，禁止客户端指定金额）
退款事实与支付事实分表；每次资金变动写不可变流水
```

**非目标**

- 不做部分退款、退货物流、真实资金渠道、佣金结算、超时自动同意。
- 不做分布式事务；退款与订单、库存仍在同一 PostgreSQL 事务内（ADR-016）。

**交付物**

- 代码：退款状态机（表驱动 + `WHERE status=from` 条件更新）、退款表与资金流水、服务/仓储/处理器、权限（买家申请、商家裁决，非买卖双方 404）、幂等（业务幂等键 + 唯一约束）、库存回补策略（ADR-017）。
- 迁移：在现有 `000015_order_refund.*`（审计期间的并发工作）基础上补齐，确认 up/down 与不变量后接管；新增回补所需的唯一约束。
- 契约与前端：OpenAPI（含 409/404 分支）、订单详情退款进度与"模拟"标记、申请与审核入口。
- 测试：真实 PG 并发测试（重复申请只退一次、退款 vs 发货/确认完成竞争、回补一次、非买卖双方越权无副作用）、Playwright 主链路、对账所需的 SQL 断言。
- 文档：`docs/commerce-business.md` 更新业务范围；`docs/commerce-v1-technical-design.md` 或新文档记录状态机与资金流水；`interview-notes/` 新增退款并发讲解。

**验收证据**

- 重复退款请求只产生一条成功退款与一条流水；退款与履约竞争只产生合法状态组合。
- 退款后库存、订单、作者统计与资金流水四处可对账（差异 0），GMV 口径与 ADR-017 一致。
- 非买卖双方操作返回 404；失败可安全重试（幂等键命中返回同一结果）。
- `TestOpenAPISync` 通过，前端 `pnpm typecheck` 通过（含新状态类型）。

**依赖关系**

依赖阶段 A（事务边界、错误语义、幂等约定）；BE-P2-12 必须先收口，否则回补会新增第四个库存写入口。可与阶段 D 并行，但 `OrderRefunded` 事件必须在退款状态机定型后再接。

**风险与回滚**

- 迁移 `000015` 是审计期间的并发成果，未经验收 → 先审阅其 up/down、约束与状态枚举，必要时重写而不是打补丁。
- 回滚退款：先禁止新申请并终止在途申请，再回滚迁移；已发生的退款事实与流水按只读历史保留（不允许删账）。
- GMV 口径从"总额"改为"净额"会影响既有归因查询与前端展示 → 口径切换需要一个明确的切换提交与文档说明，避免两套口径并存。

**提交信息建议**

```text
feat(order): add full refund request state machine
feat(order): execute mock refund with immutable ledger
feat(inventory): restore stock through the single inventory write path
fix(feed): invalidate following feed cache on video state change
feat(web): add refund request and decision flows
test(order): prove refund executes once under concurrency
docs(openapi): document refund endpoints and error branches
```

### 阶段 D：Transactional Outbox 与消息队列

**范围**

支付成功后的非核心副作用（归因投影、通知、后续统计）改为可靠事件分发：支付事务同写 `outbox_events`，relay 投递，消费者幂等，失败进 DLQ 且可重放。transport 抽象为接口，**默认 Redis Streams 实现，Kafka 作为可选实现**（ADR-014）。

**要修的审计条目**

新增能力，无对应审计编号；它同时是能力缺口 G-2 的解法，并为阶段 E 的指标与演练提供对象。

**事件与接口**

```text
topic: commerce.order-events.v1
events: OrderPaid（必须）；OrderRefunded（阶段 C 定型后追加）
transport 接口：Publish / Consume / Ack / DeadLetter / Replay
默认实现：Redis Streams（XADD / XREADGROUP / XACK / XAUTOCLAIM）
可选实现：Kafka（仅在需要分区、长期留存或多消费组生态时启用）
```

**非目标**

- 不用 MQ 替代数据库事务；不做两阶段提交或 Saga 支付（ADR-016）。
- 不拆服务；不做事件溯源；不迁移播放事件（M5-Kafka 仍搁置：已压测证明同步写不是读路径瓶颈，这是与支付侧不同的决策）。
- 支付成功不依赖 MQ 可用：MQ 停机时支付必须照常成功。

**交付物**

- 代码：`outbox_events` 表与写入（与支付同一事务）、relay（`FOR UPDATE SKIP LOCKED` + 状态条件更新，多实例不重复发布）、消费者骨架与 `event_id` 唯一约束幂等、DLQ、重放命令（带审计记录）、transport 抽象与两个实现。
- 指标：`outbox_pending`、`outbox_publish_lag`、`consume_lag`、`dlq_count`、重放次数。
- 测试与演练脚本：四个故障实验的可重复步骤。
- 文档：`docs/architecture-decisions.md` 落成 ADR-014；`interview-notes/` 新增 Outbox 讲解。

**验收证据（四个故障实验）**

1. MQ 停机时支付成功，事件留在 Outbox；MQ 恢复后自动补发，`OrderPaid` 最终恰好被消费一次。
2. 消费者处理完成、提交 offset 前崩溃 → 消息重复投递，但 GMV/统计不重复增加（唯一约束生效）。
3. 永久失败事件进入 DLQ，可安全重放且重放不产生重复副作用。
4. relay 多实例并发运行，同一事件只被发布一次。

**依赖关系**

依赖阶段 A（事务与错误语义）。与阶段 C 可并行；`OrderRefunded` 事件需等 C 的状态机与流水定型。阶段 E 的指标与演练依赖 D 的产物。

**风险与回滚**

- Redis 已承载缓存与限流，Streams 可能争用容量 → 用独立 DB index 或独立实例，并在文档写明容量隔离；回滚为关闭 relay 与消费者（Outbox 表保留，支付不受影响）。
- 消费延迟无指标时问题不可见 → 先落指标再接真实消费者。
- 重放可能造成重复副作用 → 重放必须走与首次消费完全相同的幂等路径，且记录操作审计。

**提交信息建议**

```text
feat(outbox): persist order events in the payment transaction
feat(outbox): relay events with skip locked and conditional update
feat(mq): add transport abstraction with redis streams implementation
feat(mq): consume events idempotently by event id
feat(mq): dead letter queue and audited replay command
test(outbox): prove delivery survives broker outage and consumer crash
```

### 阶段 E：对账与可证明性

**范围**

把前四个阶段的正确性变成可持续验证的证据：对账、业务指标、CI、下单链路压测、故障演练。

**覆盖**

能力缺口 G-3（对账）、G-4（业务指标）、G-5（CI）、G-6（下单压测）、G-7（故障演练）。

**交付物**

- **对账**：只读对账命令（Go 命令或 SQL 脚本），覆盖库存余额 vs 流水、已支付订单 vs 支付事实、取消/过期回补次数、作者 GMV vs 订单事实、退款 vs 资金流水、Outbox 状态分布；输出人类可读报告与机器可解析结果。
- **指标**：业务 collector（订单成功/失败、库存不足、幂等重放、状态冲突、关单积压、退款重试/失败、对账差异、Outbox backlog/消费延迟/DLQ），并在 README 说明含义与阈值来源。
- **CI**：`.github/workflows/ci.yml` 按 1.3 第 1 条落地；隔离 PG 与 Redis 用 compose service；e2e 在 CI 使用独立库与造数 fixture。
- **压测**：扩展 `docs/performance-baseline.md`，加入下单链路（创建订单、重复幂等请求、支付、取消/到期竞争、退款申请/裁决）与热点/散列 SKU 对照。
- **演练**：PostgreSQL、Redis、MinIO、MQ/Consumer（或 Worker 重启）四类故障；记录用户可见表现、指标变化、恢复步骤、数据一致性结论。

**非目标**

- 不追求真实 SLA 与容量承诺；不引入分布式 tracing（除非排障数据证明需要）；不搭混沌工程平台；对账不自动改写历史数据。

**验收证据**

- 对账报告在正常数据下差异为 0；人为制造差异（例如手动改一行库存）能被报告定位到具体 SKU/订单。
- CI 在 main 上必过，且包含阶段 A/B 的全部门禁；首次接入暴露的历史 flaky 必须有明确处理（修复或标记）。
- 压测记录固定环境、脚本、成功率、P95/P99、锁等待与连接池等待。
- 四次演练各有独立文档记录，且至少一次验证"支付在依赖故障下仍成功"。

**依赖关系**

依赖 A、B、C、D；对账与压测可以在 C 完成后先跑（不依赖 D），Outbox 相关指标与演练需 D 完成。

**风险与回滚**

- CI 首次接入会暴露历史 flaky 与全新检出问题 → 分两步：先"报告不阻塞"，再"必过"；e2e 可以先夜间跑。
- 对账 SQL 在大表上有开销 → 限制时间窗、只读连接、避免全表排序。
- 指标基数爆炸（按订单/SKU 打标签）→ 只允许低基数字段作为标签，SKU/订单 ID 只进日志。

**提交信息建议**

```text
feat(reconcile): add read-only reconciliation report
feat(metrics): expose commerce business counters
ci: run go, race, pg integration, contract and web checks
perf(order): add checkout path load baseline
docs: record dependency failure drills and recovery steps
```

### 阶段 F：文档与面试交付

**范围**

把 A–E 的证据整理成可独立阅读、可被追问的材料。

**交付物**

- 根 `README.md`：项目定位、架构图、主链、启动方式、压测结果、故障实验、已知边界。
- 面试讲解主文档（`docs/interview-*.md` 或 `docs/interview-notes/` 汇总）：三条主故事 + 退款/对账两条补充，每篇按"问题 → 排查 → 方案 → 验证"组织，结论回指代码/测试/数据。
- ADR：本计划的取舍正式落成 ADR-014..ADR-020（见第 4 节摘要）。
- `docs/README.md` 导航、`AGENTS.md` 里程碑与关键约定更新。
- `docs/interview-notes/` 为每个阶段新增讲解笔记（含失败与取舍，不只写成功路径）。

**非目标**

- 不写简历话术润色；文档中不允许出现"已完成"但无验收证据的功能。

**验收证据**

- 任取一条面试结论，能在 30 秒内指出对应的代码位置、测试或数据。
- `docs/` 与 `README` 中不再存在审核报告 CT-6 类的"文档自称有、实际没有"的表述。

**依赖关系**

依赖 A–E；可以在每个阶段结束时增量更新，避免最后集中补文档。

**风险与回滚**

- 文档滞后于代码 → 把"更新文档"写进每个阶段的完成定义，而不是单独一个收尾阶段。
- 讲解夸大未验收能力 → 文档评审时逐句核对验收证据。

## 4. ADR 摘要（待正式落成）

以下决策在本计划中生效；正式文件落成时编号以 `docs/architecture-decisions.md` 为准（现有最新为 ADR-013）。

### ADR-014：事件传输默认 Redis Streams，Kafka 作为可选实现

- **背景**：项目已有 Redis（6380）承担缓存与限流，没有 Kafka 运维能力；`M5-Kafka` 已因"同步写不是读路径瓶颈"搁置；引入 MQ 的真实动机是支付后副作用的可靠分发，不是虚构吞吐瓶颈。
- **决策**：定义 transport 接口，默认实现 Redis Streams；Kafka 仅作为可选实现，在需要分区、长期留存或多消费组生态时启用。可靠性由 Outbox + `event_id` 唯一约束保证，而不是由 transport 保证。
- **后果**：运维成本低、可立刻做故障演练；放弃 Kafka 的分区再均衡与生态工具；容量与缓存/限流共用实例，需要 DB index 或实例隔离；因为可靠性不依赖 transport，未来替换实现不需要改业务语义。

### ADR-015：退款只做全额（整单）

- **背景**：部分退款需要按订单项拆分金额、处理运费/优惠分摊与多次退款累计校验；Commerce v1 只支持单 SKU 订单，尚无优惠与运费模型。
- **决策**：退款金额等于订单总额，从订单快照派生，禁止客户端指定金额；部分退款与退货物流不做。
- **后果**：金额校验与对账口径简单、可被并发测试锁死；真实电商的按项退款需要重做金额模型，届时属于新的 ADR，不在本计划内。

### ADR-016：不做分布式事务，保持单库事务 + Outbox + 幂等消费

- **背景**：订单、支付、库存、退款都在同一 PostgreSQL 库中；审计确认现有条件更新 + 行锁 + DB CHECK 已能保证不超卖与不重复回补。
- **决策**：拒绝为"看起来更高级"引入两阶段提交或 Saga 作为支付/退款路径；跨进程的副作用通过 Outbox 异步分发，用幂等消费兜底。
- **后果**：换取同库事务的强一致与可测试性；代价是未来若拆库，必须重新设计一致性边界（届时以 ADR 记录，不提前做）。

### ADR-017：退款后的库存回补与 GMV 口径

- **背景**：审计与业务文档都指出"退款是否回补库存、GMV 记总额还是净额"必须有唯一规则，否则订单、库存、归因三处会各说各话。
- **决策**：未发货（`PAID`）退款回补库存并写回补流水；已进入 `FULFILLING`/`COMPLETED` 的退款不回补库存（货已发出）。GMV 统一按净额口径：已支付金额 − 已退款金额，作者统计与对账使用同一表达式。
- **后果**：三处口径一致、可对账；已发货退款会造成"钱退了、货在外面"的账实差异，这是业务上接受的模拟简化，必须在业务文档显式写明。

### ADR-018：refresh token 暂留 localStorage，只做多标签同步

- **背景**：FE-P1-3 同时包含两个问题：多标签互踢（必然发生）与 XSS 可读 refresh token（有条件发生）。迁移到 HttpOnly Cookie 需要后端新增 Cookie 分支、CSRF 防护与前端改造，改动面大于本阶段范围。
- **决策**：阶段 B 只做 `BroadcastChannel`/storage 同步与单飞刷新；localStorage 存储维持现状，但记录为已知风险。
- **后果**：多标签掉线被修复；XSS 风险敞口仍在，触发条件（引入第三方脚本、渲染用户富文本、出现 XSS 报告）满足时必须启动 Cookie 迁移。

### ADR-019：热点库存不引入 Redis 预扣，保持同库条件更新

- **背景**：`commerce-evolution-plan.md` C3 与性能计划都要求"先测量再演进"；当前没有下单链路的吞吐与行锁等待数据。
- **决策**：本计划不引入 Redis 预扣/秒杀链路；先用阶段 E 的压测给出热点 SKU 与散列 SKU 的对照数据，收益与补偿成本可量化后再单独立项。
- **后果**：避免为不存在的瓶颈引入补偿、对账与超卖风险；代价是面对"如何支撑秒杀"的问题时，回答是数据驱动的演进方案而不是已实现的系统。

### ADR-020：统一不可见资源返回 404（含删除他人视频）

- **背景**：BE-P2-1 指出 `DELETE /videos/{id}` 对非作者返回 403，与项目"不可见一律 ErrNotFound"的既有约定（ADR-009）冲突，且 403 会泄露 DELETED/BLOCKED 视频的存在性；现有 OpenAPI 与测试已把 403 固化。
- **决策**：统一为 404，同步 OpenAPI、`internal/video/service_test.go:477` 与前端错误分支；需要区分"已认证但无权"的排障信息只进日志，不进响应。
- **后果**：可见性与存在性语义一致、不可枚举探测；代价是客户端无法从状态码区分"不存在"与"无权限"，这是有意的安全取舍。

## 5. 与面试讲解的对应关系

每个阶段至少产出一条可完整讲述的"问题 → 排查 → 方案 → 验证"，禁止只讲方案。

| 阶段 | 可讲故事 | 问题 → 排查 → 方案 → 验证 | 产出位置 |
|---|---|---|---|
| A | 限流为什么限不住 | 伪造 `X-Forwarded-For` / 空白前缀换桶 → 读 hertz `defaultTrustedCIDRs` 与 service 的 TrimSpace → 可信代理配置 + key 归一化 → 伪造头不换桶的回归测试 | `interview-notes/` 新增限流笔记 |
| A | 超时订单为什么还能支付 | 关停 Worker 后订单仍可支付 → 读 `Pay` 条件与 Worker 吞吐上限（100 条/5s）→ 锁内加 `expires_at` 谓词 → 超时支付被拒的集成测试 | 订单并发笔记补充 |
| A | singleflight 如何从保护变成放大器 | leader 取消导致整批 500 → 读闭包 ctx 捕获与 singleflight 不缓存错误的语义 → `WithoutCancel`/独立 ctx → 缓存错误注入测试 | `feed-singleflight-isolation.md` 更新 |
| B | 契约漂移为什么 CI 没发现 | 改 schema 测试仍绿、加合法 parameters 反而红 → 读 `main_test.go` 只比 path → 方法白名单 + schema 级测试 + OpenAPI lint → 复现两个变异实验 | 新增契约测试笔记 |
| B | 本地能跑、手机和 CI 必挂 | 手机访问商品页白屏、全新 clone typecheck 18 错 → `crypto.randomUUID` 只在安全上下文 + `typecheck` 不跑 gen → fallback + 类型入库 + CI 漂移检查 → 全新 clone 直接通过 | `web/README.md` + 笔记 |
| C | 退款怎么只退一次、怎么不和发货打架 | 重复申请与并发履约 → 行锁 + 条件状态转移 + 业务幂等键 → 真实 PG 并发实验（重复申请、退款 vs 完成、回补一次） | 新增退款笔记 |
| C | 退款之后 GMV 怎么算 | 三处口径不一致 → 审计指出归因与库存各自定义 → ADR-017 净额口径 + 回补规则 → 对账报告归零 | ADR-017 + 对账文档 |
| D | 支付成功后的事件怎么不丢不重 | MQ 停机/消费者崩溃 → Outbox + relay + 唯一约束幂等消费 → 四个故障实验与 DLQ 重放 | 新增 Outbox 笔记（回填 `interview-production-readiness-plan.md` 阶段 4） |
| D | 为什么不用 Kafka | 被问"为什么不用主流 MQ" → 对比运维成本与真实需求 → ADR-014 transport 抽象 + Redis Streams 默认 → 换实现不改业务语义的证明 | ADR-014 |
| E | 怎么证明库存账实一致 | 被问"你怎么知道数据是对的" → 只有单次并发实验、没有持续对账 → 对账命令 + 差异定位 → 人为造差异被定位 | `performance-baseline.md` + 对账文档 |
| E | 依赖故障时用户看到什么 | PG/Redis/MinIO/MQ 逐个停 → 指标与日志定位 → 恢复步骤 → 四次演练记录 | 演练文档 |
| F | 三条主故事串讲 | Feed 性能演进、订单库存一致性、Outbox 可靠事件；补充退款闭环与对账 | README + 面试主文档 |

## 6. 与现有计划的关系与维护约定

- **不替换**现有计划：`interview-production-readiness-plan.md` 的阶段 4/5/6 与本计划 D/E/F 是同一件事，本文给出更细的条目编号与验收证据；`commerce-evolution-plan.md` 的 C0/C1/C2 分别落在本文 E/C/E。
- **状态回填**：每完成一个阶段，在 `docs/README.md` 的当前进度与 `AGENTS.md` 的里程碑表中更新状态与实现提交；未验收的功能不得标记完成。
- **提交纪律**：沿用 `interview-production-readiness-plan.md` 第 2 节——单主题提交、实现与测试可同提交、每阶段配面试讲解、工作区保持可解释。
- **编号稳定**：本文与审核报告共用编号；条目重命名或合并时保留原编号并标注（例如 `BE-P2-4（并入阶段 C）`），不重新编号。
