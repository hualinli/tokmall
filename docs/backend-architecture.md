# 后端架构与模块依赖图

> 本文档描述 `internal/` 各模块的依赖关系与数据所有权。
> 核心原则：**接口定义在消费侧（依赖倒置），main.go 是唯一装配点；
> 模块间不共享类型，只读共享表时用各自的 read model / 直接 SQL。**

## 1. 分层总览

```text
┌────────────────────────────────────────────────────────────────┐
│  入口 / 装配层                                                  │
│                                                                │
│  cmd/api/main.go         唯一装配点 + 路由注册（public/protected）│
│  internal/api            OpenAPI 契约（embed openapi.yaml）    │
│  cmd/api/main_test.go    route-sync 测试（契约 ↔ 路由双向 diff）│
└──────────────┬─────────────────────────────────────────────────┘
               │ 挂载中间件
┌──────────────▼─────────────────────────────────────────────────┐
│  中间件层                                                      │
│  middleware/auth      Bearer 校验 → 注入 user_id/role           │
│  middleware/recovery  panic → 统一错误响应                      │
│  middleware/requestid UUID v7（透传）                          │
│    └→ 依赖 token（验签）、response（Abort）                     │
└──────────────┬─────────────────────────────────────────────────┘
               │ handler 层调用
┌──────────────▼─────────────────────────────────────────────────┐
│  业务域层（模块化单体：模块间不共享类型）                        │
│                                                                │
│  ┌───────┐    ┌───────┐    ┌───────────┐    ┌───────┐         │
│  │ user  │    │ video │    │interaction│    │ feed  │         │
│  └───┬───┘    └───┬───┘    └─────┬─────┘    └───┬───┘         │
│      │      ┌─────┘               │             │             │
│      └──►接口◄──┘                 │             │             │
│         （见 §3 装配矩阵）          │             │             │
│                                    ▼             │             │
│                         各模块 service ← GormRepository（db）  │
└──────────────┬─────────────────────────────────────────────────┘
               │ 消费侧接口（实现由 main 注入）
┌──────────────▼─────────────────────────────────────────────────┐
│  基础设施层（无业务依赖）                                       │
│                                                                │
│  storage  MinIO（预签名 PUT/GET、Stat、Delete）                 │
│  cache    Redis client（main 持有，按接口分发）                 │
│  token    JWT Manager（access/refresh 类型区分）                │
│  database GORM + 连接池 + migrate                               │
│  response BizError 统一错误码 + HTTP 映射                       │
│  config   环境变量（默认值 + required）                         │
│  observability Prometheus collector（db_pool / feed_cache）    │
│  health   存活 / 就绪探针                                       │
└────────────────────────────────────────────────────────────────┘
```

## 2. 业务模块依赖图（箭头 = 依赖方向）

```text
  user ──────────► token          （JWT 签发/验签）
    │      ─────► storage         （头像 URL）
    │      ─────► redis-client    （refresh 黑名单实现内部持有）
    │      ─────► video           （ImageUploadValidator：头像票证校验）
    │
  video ─────────► storage        （预签名/Stat/Delete）
    │      ─────► feed            （FeedCacheInvalidator：发布/删除后失效首页缓存）
    │
  interaction ───► storage        （列表头像 URL）
    │
  feed  ─────────► storage        （播放 URL + 头像 URL）
    │      ─────► cache/redis     （首页缓存 FirstPageCache 的 Redis 实现）
    │      ─────► observability   （CacheStats 埋点）
    │      （内部）Ranker：TimeRanker（生产）/ HotRanker（仅排序实验，未开放游标分页）
```

**要点**：
- 业务模块之间的依赖只有两条，且都是**消费侧接口 + main 装配**：
  `user → video.Service`、`video → feed.RedisFeedCache`。被依赖方不知道依赖方存在。
- 不存在 `feed → video` 或 `video → interaction` 等反向依赖：feed 需要视频/点赞/关注
  数据时**直接只读共享表**（见 §4），不调用对方代码。

## 3. 装配矩阵（依赖倒置的核心）

所有接口定义在**消费方**，实现由 `cmd/api/main.go` 注入：

| 消费侧接口 | 定义位置 | 实现（main 装配） |
|---|---|---|
| `user.TokenManager` | user/service.go | `token.Manager` |
| `user.RefreshTokenBlacklist` | user/blacklist.go | `user.RedisRefreshTokenBlacklist`（Redis） |
| `user.AvatarURLProvider` | user/service.go | `storage.Client` |
| `user.ImageUploadValidator` | user/service.go | **`video.Service`** |
| `video.Storage` | video/service.go | `storage.Client` |
| `video.FeedCacheInvalidator` | video/service.go | **`feed.RedisFeedCache`** |
| `interaction.AvatarURLProvider` | interaction/service.go | `storage.Client` |
| `feed.PlayURLProvider` | feed/service.go | `storage.Client` |
| `feed.FirstPageCache` | feed/cache.go | `feed.RedisFeedCache` |
| `feed.CacheStats` | feed/cache.go | `observability.FeedCacheStats` |
| `各模块 Repository` | 各 service.go | 各 `GormRepository` |

> 反直觉点：`feed.RedisFeedCache` 同时实现 `feed.FirstPageCache`（自己用）和
> `video.FeedCacheInvalidator`（video 用）——同一 Redis key 的读写与失效，两个接口两面。

## 4. 数据所有权与跨模块读表（read model）

| 表 | 写方（拥有） | 读方（跨模块只读，不共享类型） |
|---|---|---|
| `users` | user | video（JOIN 作者）、interaction（JOIN 列表）、feed（JOIN 作者） |
| `videos` | video | feed（Feed 查询）、user 不读 |
| `upload_tickets` | video | 仅 video |
| `follows` | interaction | user（关注计数/状态）、feed（关注 Feed 的关注列表） |
| `likes` | interaction | video（点赞数/状态）、feed（点赞数） |
| `play_events` | video（Play 写入） | feed（HotRanker 播放计数） |

- **跨模块读全部走各自的 SQL / read model**（如 feed 的 `ListFollowingIDs` 直接查
  `follows`），不 import 对方模块的 model 类型，未来拆服务时按表边界切。
- 计数不冗余：`like_count`/`play_count` 实时 `COUNT(*)` 聚合（`LikeCounts`/`PlayCounts`
  批量 IN 查询），不维护计数器行（详见 supplement §3.7 实现版）。

## 5. 单向依赖保证（无环检查）

```text
基础设施层（config/database/cache/storage/token/response/observability/health）
    ↑ 被依赖
中间件层（middleware）→ 仅基础设施
    ↑ 被依赖
业务域层（user / video / interaction / feed）
    ↑ 被依赖
装配层（cmd/api main.go）
```

- 业务模块**不反向依赖**装配层；基础设施层不依赖任何业务模块。
- 潜在环：`user → video → feed → (storage/cache/observability)` 是纯单向链；
  唯一的"反向"是接口实现方向（feed 实现 video 的接口），由 main 装配切断编译期耦合。
