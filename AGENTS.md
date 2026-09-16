# Project memory

## 项目简介

TokMall：基于 CloudWeGo 技术栈（Hertz）的短视频 Feed + 商城一体化后端（学习项目）。
目标不是一次实现 TikTok，而是通过可演进的系统掌握生产级后端能力：分层架构、状态机、
幂等、游标分页、缓存、观测性。目前已带 React 前端（web/，OpenAPI 契约驱动，含 e2e）。

当前形态：**模块化单体**。模块间不共享类型（只读共享表时用各自的 read model / 直接 SQL），
未来拆服务时按模块边界拆。

## 技术栈

- HTTP：CloudWeGo Hertz v0.10（含 hertz-contrib requestid / monitor-prometheus / logger-slog）
- ORM：GORM + PostgreSQL（golang-migrate 管理 schema，禁止 AutoMigrate）
- 对象存储：MinIO（预签名 PUT 直传 + 预签名 GET 播放 URL）
- 认证：JWT（access 15min / refresh 7d，HS256）+ Redis 黑名单（refresh 可撤销，ADR-011）
- 缓存：Redis（6380），cache-aside + singleflight + double-check（ADR-012）
- 观测：slog 结构化日志（stdout + lumberjack 轮转文件）、Prometheus /metrics（独立 9091 端口）
- 契约：OpenAPI 3（internal/api/openapi.yaml，route-sync 测试防路由漂移）
- 测试：后端 testify（require）+ hertz ut 内存链路；前端 Vitest + Playwright e2e
- 前端：React + TanStack Router + React Query + Vite + Tailwind（web/，openapi-typescript 生成类型）

## 目录结构

```text
cmd/api/main.go          服务入口：装配依赖 + 路由分组（公开组 /api/v1、受保护组）
internal/
  api/                   OpenAPI 契约（spec.go embed openapi.yaml + route-sync 测试）
  config/                环境变量配置（默认值 + required）
  database/              连接 + 连接池(25/5/1h) + migrate
  cache/                 Redis 客户端（模块不持有原始 client，main 装配注入消费侧接口）
  storage/               MinIO 封装（预签名 PUT 直传、预签名 GET 播放 URL、StatObject 翻译 NoSuchKey）
  token/                 JWT Manager + GenClaims + Verify（access/refresh 类型区分）
  middleware/            auth（Bearer 校验 + user_id/role 注入，AuthOptional 用于公开接口）、recovery、requestid
  response/              BizError 统一错误码 + HTTP 状态映射 + Abort
  health/                存活/就绪探针
  user/                  注册/登录/me + refresh 旋转 + logout 黑名单（model + repository + service + handler）
  video/                 发布/详情/发布/删除 + upload_ticket 防伪造（状态机）+ 作者视频列表 + 播放事件
  interaction/           点赞/关注（幂等，ON CONFLICT DO NOTHING）+ 关注/粉丝列表
  feed/                  Feed 游标分页（HMAC 签名游标）+ 首页缓存 + 关注 Feed（拉模式）+ Ranker 排序 + read model
  observability/         db_pool / feed_cache 指标 collector
migrations/             SQL 迁移（001 users、002 软删、003 videos+tickets、004 follows+likes、005 作者索引、006 play_events）
web/                    React 前端（TanStack Router + React Query + Vite + Playwright e2e）
scripts/                seed_perf.sql 百万数据造数（压测用）
docs/                   mentor 手册 + ADR + 性能基线（git 跟踪，内部使用）
```

## 里程碑状态

| 里程碑 | 状态 | 要点 |
|---|---|---|
| M0 工程骨架 | ✅ | Hertz + PG + 配置 + 中间件 + 日志 |
| M1 视频发布 | ✅ | MinIO 预签名 + upload_ticket + 状态机 |
| M2 Feed 分页 | ✅ | 游标 (published_at,id) + HMAC + 部分索引 |
| M3 互动幂等 | ✅ | 唯一约束 + ON CONFLICT + 并发验证 |
| L2 观测性 | ✅ | /metrics(9091) + db_pool + 基线压测 |
| M4 缓存 | ✅ | Redis(6380) + singleflight + 首页缓存：1M 数据 6.6k→11.3k QPS |
| M4+ Auth 闭环 | ✅ | /refresh 旋转 + /logout 黑名单（ADR-011，Redis 引入的附带收益） |
| M4+ 内容域扩展 | ✅ | 预签名 GET 播放 URL（route A）、作者视频列表 + 硬删 MinIO、公共主页 + 社交列表 + 点赞数 |
| M4+ 契约与前端 | ✅ | OpenAPI 契约 + route-sync 测试 + React 前端（TanStack Router）+ Playwright e2e |
| M5 播放事件（同步版） | ✅ | play_events 表 + POST /videos/{id}/play：event_id 幂等（UUID v7）、仅 PUBLISHED、未来时间拒绝；Kafka 异步化待做 |
| M6 关注 Feed | ✅ | 拉模式：窗口函数每作者 TOP 5（无 N+1）+ 时间游标下推 + 空关注短路；HotRanker 仅保留为包内排序实验 |
| M8 排序演进 | ✅（边界已收口） | Ranker 接口 + TimeRanker 生产游标分页；HotRanker 仅单元实验（排序键未与游标闭合，配置 fail-fast 禁止启用）；HybridRanker 待做 |
| M7 限流 | ✅ | Redis 固定窗口（lua INCR+EXPIRE）+ 三档规则（auth 10/min/IP、read 200/10s/IP、write 100/10s/user）+ fail-open 降级 + RATE_LIMITED 429 |
| M5-Kafka | 搁置 | play 事件异步化（Kafka + Worker）：已压测证明当前同步写不是读路径瓶颈；若未来写吞吐成为瓶颈再引入 |

> 注：M4+ 三行不在原里程碑规划内（按 mentor 手册"出现可测量问题再引入技术"，这些是 M4 的
> 自然延伸：Redis 引入后顺手补 auth 可撤销性，内容域扩展是前端联调需求驱动）。
> M5/M6/M8/M7 为方向 1 + 治理交付（详见 docs/feed-system-mentor-guide.md §9-12、§7），
> 下一步候选：Commerce v1（先做普通商品/订单/视频挂商品/作者带货归因，业务梳理见 docs/commerce-business.md，
> 技术指南见 docs/content-commerce-backend-agent-guide.md）。M5-Kafka 已通过压测搁置，不再作为近期候选。

## 关键设计约定（改代码前必读）

- **错误三层链**：repo 翻译 ORM 错误为领域哨兵 → service 判定分类+日志分级（业务失败不打 Error）→ handler 映射 BizError，未知一律 500。接口定义在消费侧。
- **状态机**：video 模块表驱动 `statusTransitions` + DB 条件更新（`WHERE status=from`）防并发。
- **可见性**：DELETED 对所有人 404，未发布仅作者可见，不可见一律 ErrNotFound（不泄露存在性）。
- **幂等**：关系表唯一约束 + `ON CONFLICT DO NOTHING`，结果语义固定（like→true）。
- **游标**：表示上一页最后一条（非当前时间），防翻页重复；rank_version 失效旧游标。只有排序键与游标一致的排序器才能上线；当前生产关注 Feed 仅支持 time-v1，HotRanker 不能通过配置或 Service 启用。
- **分层数据流**：handler(HTTP DTO) → service(领域 Params) → repo(model)，model 永不出 service（PasswordHash json:"-" 级别防护）。
- **列名/状态类型化**：`column`/`Status` 自定义类型防拼错；到 GORM 边界必须显式转 string。
- **连接池**：max_open=25, max_idle=5, lifetime=1h（M3 基线接近饱和；M4 缓存命中后 db_pool_wait_count 归零，瓶颈已移到 Redis 读，见 performance-baseline.md）

## 常用命令

```bash
make dev                                    # 一键：依赖容器 + 后端 + 前端（全部后台，日志 logs/）
make stop                                   # 停前后端（幂等）
make dev-backend / make dev-web             # 只起后端 / 只起前端（JWT_SECRET 等已内置，可 make dev JWT_SECRET=xxx 覆盖）
tail -f logs/api.log logs/web.log           # 看日志
go test ./...                               # 后端全量测试
cd web && pnpm test:e2e                     # Playwright e2e（需后端 + 依赖容器在跑）
curl localhost:9091/metrics                 # 指标
wrk -t4 -c50 -d10s http://localhost:8080/api/v1/feed   # 压测（基线见 docs/performance-baseline.md）
```

## 环境陷阱

- PostgreSQL 标准端口 5432（docker-compose 已映射）；本机 6379 被 go-oj 占用，Redis 走 6380（compose 已映射）。
  **不要额外创建 5433 之类的 PG 实例**：早前文档里的 5433 e2e 实例是 5432 被临时占用时手动 `docker run` 的权宜之物，非标准配置，已清理。
- 重启服务：`make stop && make dev`（后端即 bin/api 进程；**不要用 `lsof -ti :8080 | xargs kill -9`**——本机
  Shadowrocket/微信也监听 8080，会一起误杀；`make stop` 只匹配 bin/api 和 vite 进程）。
  go run 子进程名会变，压测一律用 `make build` 的 `./bin/api` 起服务。
- macOS 无 `setsid`，后台起服务用 `nohup ... & disown`，且 E2E 尽量在同一命令内完成。
- `docs/`、`AGENTS.md` 由 git 跟踪（早前误写为 gitignore，已修正）：AGENTS.md 是项目记忆、
  docs/ 是 mentor 手册 + 压测基线等内部文档，内容偏本项目使用，不面向外部读者。

## 工作方式（Mentor 模式）

本项目由一个资深后端导师（AI）指导开发。指导原则：

- 少写具体代码，多教思路：分层、错误处理、并发、性能、安全等生产级知识。
- 先给计划 → 用户审核 → 按步骤执行，每步完成都要解释"为什么这样写"。
- 用户写代码后负责 review，按 P0（崩溃/安全）→ P1（正确性）→ P2（质量）排序指出问题。
- 若用户设计中存在"学生思维/玩具项目思维"的严重缺陷（分布式事务、网络安全、性能瓶颈、
  认证绕过等盲区），**主动中断并发红色警告**，给出工业级解法。
- 用户确实写不出来时才直接给代码，且要给可抄的完整实现。
- 避免"顺从性陷阱"：不为了附和用户而同意错误设计，每个里程碑的简化决策要显式记录（ADR）。
