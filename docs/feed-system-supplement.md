# Feed 系统补充设计与决策记录

> 本文件保留 Feed 早期 API/DDL 草案、认证演进设想和 ADR 推演，**不是当前接口契约**。
> 现行路由与 DTO 以 [`OpenAPI`](../internal/api/openapi.yaml) 为准，schema 以 [`migrations`](../migrations/) 为准，
> 进度见 [文档导航](README.md)。阅读下文的“当前”“后续”均需按原写作时点理解。

---

## 1. 认证方案

### 1.1 V1：JWT Bearer Token

**决策**：Feed v1 使用无状态 JWT。V2 演进为 Session + Redis。

**原因**：
- V1 无状态，不需要额外部署 Redis 会话存储（Redis 的引入应绑定在缓存需求上）
- 接口测试简单：直接传 token 即可，无需处理 cookie
- JWT payload 可直接传递 `user_id`、`role`，减少一次数据库/缓存查用户
- 面试时可以作为"有状态 vs 无状态"的对比案例，V2 重构时有明确的动机

**Token 设计**：

```json
// Access Token (15 分钟过期)
{
  "sub": "user_id",
  "role": "USER",
  "type": "access",
  "iat": 1690000000,
  "exp": 1690000900
}

// Refresh Token (7 天过期，存 Redis 黑名单)
{
  "sub": "user_id",
  "type": "refresh",
  "kid": "key-version-v1",
  "iat": 1690000000,
  "exp": 1690600000
}
```

**签名**：HMAC-SHA256，密钥从环境变量 `JWT_SECRET` 读取。V2 演进时切换到 RSA 非对称密钥对。

**测试与实现**：JWT 中间件构造函数接收 `secret []byte` 参数——生产代码从环境变量读取，集成测试直接传固定字符串（如 `"test-secret-not-for-production"`），不依赖运行环境。这样 Testcontainers 集成测试可脱离环境变量独立运行。

**Token 传递**：客户端在 HTTP Header 中传递：

```
Authorization: Bearer <access_token>
```

**刷新流程**：

```
POST /api/v1/auth/refresh
  Header: Authorization: Bearer <refresh_token>
  Response: { "access_token": "...", "refresh_token": "...", "expires_in": 900 }
```

**中间件职责**（Feed v1）：

1. 从 `Authorization` header 提取 token
2. 校验 HMAC 签名和过期时间
3. 解析 `sub` 作为 `viewer_id` 注入 context
4. token 缺失或无效 → 401 `UNAUTHENTICATED`
5. 将 `viewer_id` 和 `role` 注入 `context`（Go 标准 context）

### 1.2 V2 演进记录

| 版本 | 方案 | 动机 |
|---|---|---|
| V1 | JWT（HMAC-SHA256） | 快速起步，无状态，测试友好 |
| V2 | Session + Redis | 需要主动踢人、token 级权限变更、刷单拦截 |

V2 切换时保留 JWT 解码器作为降级路径。

### 1.3 参考实现：JWT 认证中间件（Go/Hertz）

使用 `github.com/golang-jwt/jwt/v5` 解析，中间件构造时注入 `secret []byte`（生产从环境变量读取，测试传固定值，见 §1.1"测试与实现"）。

```go
// internal/middleware/auth.go
package middleware

import (
    "context"
    "errors"
    "net/http"
    "strings"

    "github.com/cloudwego/hertz/pkg/app"
    "github.com/golang-jwt/jwt/v5"

    "github.com/lihualin/tokmall/internal/response" // 见 §5.4
)

// Claims 对应 §1.1 的 token payload
type Claims struct {
    Role string `json:"role"`
    Type string `json:"type"` // access / refresh
    jwt.RegisteredClaims
}

// Auth 返回认证中间件
func Auth(secret []byte) app.HandlerFunc {
    return func(ctx context.Context, c *app.RequestContext) {
        header := string(c.GetHeader("Authorization"))
        tokenStr, ok := strings.CutPrefix(header, "Bearer ")
        if !ok {
            response.Error(c, response.ErrUnauthenticated)
            c.Abort()
            return
        }

        token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
            if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
                return nil, errors.New("unexpected signing method")
            }
            return secret, nil
        })
        if err != nil || !token.Valid {
            response.Error(c, response.ErrUnauthenticated)
            c.Abort()
            return
        }

        claims := token.Claims.(*Claims)
        c.Set("user_id", claims.Subject)
        c.Set("role", claims.Role)
        c.Next(ctx)
    }
}
```

**使用说明**：

- 注册/登录 handler 用同一 `Claims` 结构签发 token 对：access 15 分钟、refresh 7 天，`Type` 字段区分。
- Handler 取当前用户：`userID, _ := c.GetInt64("user_id")`；`c.GetString("role")`。
- 除 `/auth/*` 外全部挂载：`h.Group("/api/v1", middleware.Auth(secret))`，`/auth/*` 单独注册在无鉴权分组。
- V2 切 Session 时，替换中间件内部实现，handler 侧 `GetInt64("user_id")` 不变。

---

## 2. API 路由设计

### 2.1 路由总表

> ⚠️ 本表是 V1 设计期快照。**权威路由来源是 `internal/api/openapi.yaml`**（有
> route-sync 测试保证与代码一致），下表与实际演进有出入（如 `POST /videos/{id}/play`
> 设计为 V1 但实际推迟到 M5；`/auth/logout`、`/me`、`/users/{id}/following|followers`、
> `/videos/me` 是设计后新增）。改动 API 时只改 openapi.yaml，不要改本表。

Base path: `/api/v1`

#### 认证

| 方法 | 路径 | 说明 | V1 范围 |
|---|---|---|---|
| POST | `/auth/register` | 注册新用户 | ✅ |
| POST | `/auth/login` | 登录，返回 token 对 | ✅ |
| POST | `/auth/refresh` | 刷新 token | ✅ |

#### Feed

| 方法 | 路径 | 说明 | V1 范围 |
|---|---|---|---|
| GET | `/feed` | 全局时间排序 Feed | ✅ |
| GET | `/feed/following` | 关注 Feed（M6） | ❌ V1 不做 |
| GET | `/users/{user_id}/videos` | 作者主页视频列表 | ✅ |

#### 视频

| 方法 | 路径 | 说明 | V1 范围 |
|---|---|---|---|
| POST | `/videos/upload-url` | 获取预签名上传 URL | ✅ |
| POST | `/videos` | 创建视频记录 | ✅ |
| GET | `/videos/{id}` | 视频详情（含商品 ID） | ✅ |
| DELETE | `/videos/{id}` | 删除视频 | ✅ |
| POST | `/videos/{id}/publish` | 发布视频（审核通过） | ✅ |

#### 互动

| 方法 | 路径 | 说明 | V1 范围 |
|---|---|---|---|
| POST | `/videos/{id}/like` | 点赞（幂等） | ✅ |
| DELETE | `/videos/{id}/like` | 取消点赞（幂等） | ✅ |
| POST | `/users/{id}/follow` | 关注（幂等） | ✅ |
| DELETE | `/users/{id}/follow` | 取消关注（幂等） | ✅ |
| POST | `/videos/{id}/play` | 上报播放事件 | ✅ |

### 2.2 通用约定

- **时间格式**：全部使用 RFC3339 UTC，如 `2026-07-30T08:00:00Z`。
- **ID 类型**：`user_id` / `video_id` / `product_id` 均为 int64。
- **分页参数**：`limit` 默认 20，合法范围 1-100；`cursor` 可选，必须为带签名的游标串。
- **鉴权**：除 `/auth/*` 三个端点外，所有端点都需要 `Authorization: Bearer <access_token>`。
- **幂等键**：创建类写操作（`POST /videos/upload-url`、`POST /videos`）客户端可携带 `Idempotency-Key` header。服务端在幂等窗口内（5 分钟）对同一 key 返回相同结果；重复且结果不一致时返回 `409 DUPLICATE_ACTION`。
- **请求 ID**：客户端可传 `X-Request-ID`；缺失时服务端生成 UUID v7。响应头 `X-Request-ID` 回传，日志和错误响应的 `request_id` 与之对应。
- **错误响应**：统一结构 `{ "error": { code, message, details? }, "request_id" }`，错误码与 HTTP 状态码映射见 §5.2。
- **成功响应**：不包 envelope，直接返回资源对象或分页结构。

### 2.3 认证端点

#### POST /auth/register — 注册

请求：

```json
{
  "email": "user@example.com",
  "username": "demo_user",
  "password": "S3cure-Passw0rd!"
}
```

字段校验规则：

| 字段 | 规则 |
|---|---|
| `email` | RFC5322 宽松格式校验，≤255 字符，服务端统一转小写存储 |
| `username` | 3-64 字符，仅 `[a-zA-Z0-9_]` |
| `password` | 8-72 字节（bcrypt 输入上限 72 bytes） |

响应 `201 Created`（注册即签发 token，简化客户端流程）：

```json
{
  "user": {
    "id": 1,
    "username": "demo_user",
    "avatar_url": null,
    "bio": ""
  },
  "access_token": "eyJhbGciOiJIUzI1NiIs...",
  "refresh_token": "eyJhbGciOiJIUzI1NiIs...",
  "expires_in": 900
}
```

错误：`400 INVALID_ARGUMENT`（校验失败，details 指明字段）、`409 DUPLICATE_ACTION`（email 或 username 已存在，details 指明冲突字段）。

#### POST /auth/login — 登录

请求：

```json
{
  "login": "user@example.com",
  "password": "S3cure-Passw0rd!"
}
```

`login` 同时接受 email 或 username。响应 `200 OK`，结构同 register。

错误：**`401 UNAUTHENTICATED` 统一返回**——无论用户不存在还是密码错误，都返回同一错误码和文案 `"invalid credentials"`，防止账号枚举。响应中不区分具体原因。

#### POST /auth/refresh — 刷新 token

请求：`Authorization: Bearer <refresh_token>`，body 为空。

响应 `200 OK`：

```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIs...",
  "refresh_token": "eyJhbGciOiJIUzI1NiIs...",
  "expires_in": 900
}
```

错误：`401 UNAUTHENTICATED`（refresh token 过期、签名无效或已进黑名单）。

### 2.4 Feed 端点

#### GET /feed — 全局时间排序 Feed

Query 参数：`limit`（默认 20，1-100）、`cursor`（可选）。

响应 `200 OK`：

```json
{
  "items": [
    {
      "video_id": 12345,
      "author": {
        "user_id": 9,
        "username": "demo_merchant",
        "avatar_url": "https://minio.example.local/avatars/abc.jpg"
      },
      "title": "春季外套开箱测评",
      "cover_url": "https://minio.example.local/covers/def.jpg",
      "video_url": "https://minio.example.local/videos/ghi.mp4",
      "duration_seconds": 45,
      "like_count": 128,
      "comment_count": 12,
      "is_liked": true,
      "product_ids": [456],
      "published_at": "2026-07-30T08:00:00Z"
    }
  ],
  "next_cursor": "eyJwdWJ...sIn0=",
  "has_more": true
}
```

边界行为：

- 空结果：`items: []`、`next_cursor: null`、`has_more: false`。
- 游标无效/篡改/rank_version 不匹配：`400 INVALID_CURSOR`，客户端应清空游标重新请求第一页。
- `limit > 100`：`400 INVALID_ARGUMENT`。

> **商品卡现状**：Feed 与视频详情已按页动态装配 `product_cards` DTO；下架商品保留卡片并标记不可购买。下文的 `product_ids` 是早期设计，不是当前客户端接入指南。
>
> **`is_liked`**：当前登录用户对该视频的点赞状态，由 `likes` 表批量查询填充。
>
> **`video_url` / `cover_url`**：服务端返回 MinIO 预签名 URL（短期有效，默认 1 小时），不返回内部 object_key。

#### GET /users/{user_id}/videos — 作者主页视频列表

Query 参数与响应结构同 `/feed`，`items` 仅包含该作者的已发布视频。

错误：`404 USER_NOT_FOUND`。

### 2.5 视频端点

#### POST /videos/upload-url — 获取预签名上传 URL

可选 header：`Idempotency-Key`。

请求：

```json
{
  "filename": "spring-jacket.mp4",
  "file_size": 52428800,
  "content_type": "video/mp4"
}
```

校验规则：

| 字段 | 规则 |
|---|---|
| `file_size` | 1MB - 512MB |
| `content_type` | 白名单：`video/mp4`、`video/webm`、`video/quicktime` |
| `filename` | 仅保留扩展名用于推断类型，不作为存储名 |

响应 `200 OK`：

```json
{
  "upload_id": "upl_01HZ8K2...",
  "object_key": "videos/9/01HZ8K2....mp4",
  "upload_url": "https://minio.example.local/videos/9/01HZ8K2....mp4?X-Amz-Algorithm=...",
  "expires_at": "2026-07-30T08:15:00Z"
}
```

说明：

- `upload_id` 是创建视频时的幂等凭证，与 `Idempotency-Key` 绑定。
- 5 分钟内同一幂等键返回同一 `upload_url`；过期后再次请求生成新的。
- 错误：`400 INVALID_ARGUMENT`（大小/类型）、`409 DUPLICATE_ACTION`（同幂等键在窗口内被以不同参数请求）。

#### POST /videos — 创建视频记录

可选 header：`Idempotency-Key`。

请求：

```json
{
  "title": "春季外套开箱测评",
  "object_key": "videos/9/01HZ8K2....mp4",
  "duration_seconds": 45,
  "cover_key": "covers/9/01HZ8K2....jpg",
  "product_ids": []
}
```

校验规则：

| 字段 | 规则 |
|---|---|
| `title` | ≤200 字符，非空 |
| `object_key` | **必须匹配 upload-url 阶段发放的 key**，否则 `400 UPLOAD_URL_EXPIRED`（防伪造上传） |
| `duration_seconds` | 1-3600 |
| `product_ids` | V1 接受空数组；Commerce 接入后校验商品存在性 |

响应 `201 Created`：

```json
{
  "video_id": 12345,
  "status": "PENDING_REVIEW"
}
```

错误：`400 INVALID_ARGUMENT`、`400 UPLOAD_URL_EXPIRED`。

#### GET /videos/{id} — 视频详情

响应 `200 OK`：

```json
{
  "video_id": 12345,
  "author": {
    "user_id": 9,
    "username": "demo_merchant",
    "avatar_url": "https://minio.example.local/avatars/abc.jpg"
  },
  "title": "春季外套开箱测评",
  "cover_url": "https://minio.example.local/covers/def.jpg",
  "video_url": "https://minio.example.local/videos/ghi.mp4",
  "duration_seconds": 45,
  "status": "PUBLISHED",
  "like_count": 128,
  "is_liked": true,
  "is_followed": false,
  "product_ids": [456],
  "published_at": "2026-07-30T08:00:00Z",
  "created_at": "2026-07-29T10:00:00Z"
}
```

错误：`404 VIDEO_NOT_FOUND`；非作者访问未发布视频返回 `404 VIDEO_NOT_PUBLISHED`（与不存在同语义，不泄露存在性）。

#### POST /videos/{id}/publish — 发布视频

权限：仅作者或 `ADMIN`。请求 body 为空。

响应 `200 OK`：

```json
{
  "video_id": 12345,
  "status": "PUBLISHED"
}
```

错误：`403 FORBIDDEN`、`404 VIDEO_NOT_FOUND`、`409`（非法状态转换，如从 `DELETED` 发布）。

#### DELETE /videos/{id} — 删除视频

权限：仅作者或 `ADMIN`。请求 body 为空。

响应 `200 OK`：

```json
{
  "video_id": 12345,
  "status": "DELETED"
}
```

错误：`403 FORBIDDEN`、`404 VIDEO_NOT_FOUND`。

说明：**软删除**——`status` 置 `DELETED`，发布 `VideoDeleted` 事件清理缓存和收件箱，MinIO 对象由定时任务延迟清理。

### 2.6 互动端点

互动端点全部幂等：重复调用返回相同结果，不产生额外副作用。

#### POST /videos/{id}/like — 点赞

请求 body 为空。响应 `200 OK`：

```json
{
  "video_id": 12345,
  "liked": true
}
```

#### DELETE /videos/{id}/like — 取消点赞

响应 `200 OK`：

```json
{
  "video_id": 12345,
  "liked": false
}
```

#### POST /users/{id}/follow — 关注

响应 `200 OK`：

```json
{
  "followee_id": 9,
  "following": true
}
```

错误：`400 INVALID_ARGUMENT`（自关注）、`404 USER_NOT_FOUND`。

#### DELETE /users/{id}/follow — 取消关注

响应 `200 OK`：

```json
{
  "followee_id": 9,
  "following": false
}
```

#### POST /videos/{id}/play — 上报播放事件

请求：

```json
{
  "event_id": "0191b3e1-5f0a-7f2b-9c3d-4e5f6a7b8c9d",
  "session_id": "sess_01HZ8K2...",
  "played_at": "2026-07-30T08:01:23Z"
}
```

字段规则：`event_id` 必填（客户端生成 UUID v7，服务端去重依据）；`session_id`、`played_at` 可选（缺省 `played_at` 用服务端接收时间）。

响应 `202 Accepted`（仅轻量校验后写 Kafka 立即返回，不落库）：

```json
{
  "accepted": true,
  "event_id": "0191b3e1-5f0a-7f2b-9c3d-4e5f6a7b8c9d"
}
```

错误：`400 INVALID_ARGUMENT`（event_id 格式非法）。重复 `event_id` 由 consumer 幂等去重，接口层不判重。

### 2.7 游标格式

```json
{
  "published_at": "2026-07-30T08:00:00Z",
  "video_id": 12345,
  "rank_version": "time-v1"
}
```

编码流程：JSON 序列化 → Base64 URL-safe 编码 → HMAC-SHA256 生成签名 → 拼接为 `{payload}.{signature}`。

解码流程：拆出 payload → 校验 HMAC → Base64 解码 → JSON 解析。

rank_version 变化时（如从 time-v1 切到 hot-v1），服务端比对版本号，不匹配时拒绝旧游标，告知客户端重新开始。

---

## 3. 完整数据模型 DDL

### 3.1 users

```sql
CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL,
    username VARCHAR(64) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    avatar_key TEXT,                              -- MinIO object key
    bio TEXT NOT NULL DEFAULT '',
    role VARCHAR(20) NOT NULL DEFAULT 'USER'      -- USER, MERCHANT, ADMIN
        CHECK (role IN ('USER', 'MERCHANT', 'ADMIN')),
    status VARCHAR(20) NOT NULL DEFAULT 'ACTIVE'  -- ACTIVE, DISABLED, DELETED
        CHECK (status IN ('ACTIVE', 'DISABLED', 'DELETED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_users_email UNIQUE (email),
    CONSTRAINT uq_users_username UNIQUE (username)
);

CREATE INDEX idx_users_status ON users (status);
```

**设计说明**：
- `email` + `username` 各有唯一约束，登录时支持邮箱或用户名登录
- `password_hash` 存储 bcrypt hash（成本因子推荐 10）
- `role` 在 V1 主要用于区分普通用户和内容审核者
- `avatar_key` 为 MinIO key，V1 可先为空，允许修改头像时再实现上传

### 3.2 videos（补充字段）

在 mentor 手册基础上补充商品关联相关字段：

```sql
CREATE TABLE videos (
    id BIGSERIAL PRIMARY KEY,
    author_id BIGINT NOT NULL REFERENCES users(id),
    title TEXT NOT NULL,
    object_key TEXT NOT NULL,
    cover_key TEXT,
    duration_seconds INT NOT NULL DEFAULT 0,
    status VARCHAR(20) NOT NULL DEFAULT 'UPLOADING'
        CHECK (status IN (
            'UPLOADING', 'PROCESSING', 'PENDING_REVIEW',
            'PUBLISHED', 'BLOCKED', 'FAILED', 'DELETED'
        )),
    published_at TIMESTAMPTZ,
    like_count INT NOT NULL DEFAULT 0,
    comment_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_videos_feed
    ON videos (published_at DESC, id DESC)
    WHERE status = 'PUBLISHED';

CREATE INDEX idx_videos_author
    ON videos (author_id, published_at DESC)
    WHERE status = 'PUBLISHED';
```

**修改说明**：
- 添加 `duration_seconds`（客户端上传时提供或服务端从 MinIO 元数据读取）
- 添加 `like_count` / `comment_count`（冗余计数，M3 异步聚合更新）
- 添加 `author_id` 外键引用 `users` 表
- 补充 `idx_videos_author` 索引，用于作者主页视频列表和关注 Feed 的批量查询

### 3.3 follows

```sql
CREATE TABLE follows (
    follower_id BIGINT NOT NULL REFERENCES users(id),
    followee_id BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (follower_id, followee_id),
    CONSTRAINT ck_follow_no_self CHECK (follower_id <> followee_id)
);

CREATE INDEX idx_follows_followee
    ON follows (followee_id, follower_id);
```

**`CK_FOLLOW_NO_SELF`**：禁止关注自己。

**关注上限**：服务层软限制最多关注 1000 人，关注 Feed 查询时截取最新 1000 条关注记录。

### 3.4 likes

```sql
CREATE TABLE likes (
    user_id BIGINT NOT NULL REFERENCES users(id),
    video_id BIGINT NOT NULL REFERENCES videos(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, video_id)
);

CREATE INDEX idx_likes_video ON likes (video_id, user_id);
```

### 3.5 play_events

```sql
CREATE TABLE play_events (
    event_id UUID PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    video_id BIGINT NOT NULL REFERENCES videos(id),
    session_id VARCHAR(128),
    played_at TIMESTAMPTZ NOT NULL,
    trace_id VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 按 video_id 分区（PostgreSQL 分区表或应用层按时间归档）
-- 注意：V1 可先不建索引，因为 play_events 仅用于异步聚合和排查，不用于在线查询
-- 如果确实需要按 video_id 查询聚合结果，使用聚合表而非直接查原始表
```

**设计说明**：
- `event_id` 使用 UUID 以便客户端和服务端独立生成，不依赖数据库自增
- `played_at` 区分事件发生时间和入库时间（`created_at`）
- **实现状态**：M5 前是同步写入（POST /videos/{id}/play 直接 INSERT，event_id 唯一约束幂等）；
  原 M5 异步化方案设想由 Kafka consumer 写入；当前 API 同步写入，保留/清理策略尚未实现。
  索引 idx_play_events_video_time 已建（HotRanker 的 PlayCounts 聚合在线查询用）

### 3.6 feed_cursors（可选）

```sql
CREATE TABLE feed_cursors (
    user_id BIGINT NOT NULL REFERENCES users(id),
    scene VARCHAR(64) NOT NULL,           -- 'global', 'following', 'author:123'
    cursor_data TEXT NOT NULL,             -- 序列化的游标 JSON
    rank_version VARCHAR(32) NOT NULL,
    expired_at TIMESTAMPTZ NOT NULL,      -- 游标有效期
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, scene, rank_version)
);
```

仅在需要"跨设备同步翻页位置"时才启用。V1 可以不实现，客户端管理游标即可。

### 3.7 计数聚合表（可选）

```sql
CREATE TABLE video_stats (
    video_id BIGINT NOT NULL PRIMARY KEY REFERENCES videos(id),
    like_count INT NOT NULL DEFAULT 0,
    play_count INT NOT NULL DEFAULT 0,
    comment_count INT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**决策（实现版）**：V1 实际没有用冗余字段，而是**实时批量 COUNT**——video 模块跨模块只读
likes 表（`LikeCounts(ctx, videoIDs)` 一次批量查多个视频），不维护 `video_stats` 表。
取舍：点赞写路径（M3）不碰计数行，避免了热行锁；读路径每页一次批量 COUNT 在当前量级
可接受。当并发压力增大后（计数成为读瓶颈、或需要播放/评论等多维统计），再拆为独立的
`video_stats` 表走异步事件聚合。这个演进要记 ADR。

---

## 4. MinIO 上传流程

### 4.1 完整时序

```
Client                        Server                        MinIO
  |                              |                             |
  |  POST /api/v1/videos/upload-url   |                             |
  |  Body: { filename, file_size }    |                             |
  |------------------------------>|                             |
  |                              |  1. 校验用户身份和上传权限      |
  |                              |  2. 生成 object_key:            |
  |                              |     videos/{user_id}/{uuid}.mp4 |
  |                              |  3. 生成预签名 PUT URL (15min) |
  |                              |                             |
  |  { upload_url, object_key }  |                             |
  |<------------------------------|                             |
  |                              |                             |
  |  PUT <upload_url>            |                             |
  |  Body: file binary           |                             |
  |--------------------------------------------------------->|
  |                              |                             |
  |  200 OK                      |                             |
  |<---------------------------------------------------------|
  |                              |                             |
  |  POST /api/v1/videos         |                             |
  |  Body: { title, object_key,  |                             |
  |          cover_key?,         |                             |
  |          duration_seconds }  |                             |
  |------------------------------>|                             |
  |                              |  4. INSERT videos            |
  |                              |     (status=UPLOADING)       |
  |                              |  5. 模拟转码处理             |
  |                              |     status→PROCESSING→      |
  |                              |     PENDING_REVIEW           |
  |                              |     （实际直接跳至           |
  |                              |      PENDING_REVIEW）        |
  |  { video_id, status }       |                             |
  |<------------------------------|                             |
```

### 4.2 关键规则

1. **object_key 命名**：`videos/{user_id}/{uuid}.mp4`，保证不冲突，且便于 MinIO 按用户前缀管理
2. **幂等上传**：客户端在 `upload-url` 请求中携带幂等 key，服务端 5 分钟内返回相同 URL
3. **孤儿文件清理**：定时任务扫描 `status=UPLOADING` 超过 30 分钟的记录，检查 MinIO 中是否存在对应文件。存在但记录处于 UPLOADING 则更新状态为 FAILED；不存在则删除记录
4. **V1 简化**：不实现分片上传，不实现多分辨率转码。`duration_seconds` 由客户端在创建视频时提交，服务端不做校验

### 4.3 V1 视频状态机（简化版）

```
UPLOADING ──> PROCESSING ──> PENDING_REVIEW ──> PUBLISHED
    |              |               |
    +──> FAILED <──+               |
                                   |
                   BLOCKED / DELETED
```

V1 实现中，`PROCESSING` 和 `PENDING_REVIEW` 是**立即自动转换**的，但状态机保留完整路径，以便未来插入真实审核流程。状态转换代码应集中在一处，通过配置决定是否跳过某些步骤。

---

## 5. 统一错误码

### 5.1 错误响应结构

```json
{
  "error": {
    "code": "INVALID_CURSOR",
    "message": "Cursor has expired or is malformed",
    "details": {
      "reason": "HMAC signature mismatch"
    }
  },
  "request_id": "req-abc123"
}
```

### 5.2 错误码表

| Code | HTTP | 说明 | 触发条件 |
|---|---|---|---|
| `INVALID_ARGUMENT` | 400 | 参数校验失败 | limit > 100、缺少必填字段 |
| `INVALID_CURSOR` | 400 | 游标无效 | HMAC 校验失败、payload 解码失败、rank_version 不匹配 |
| `VIDEO_NOT_FOUND` | 404 | 视频不存在 | 视频 ID 不存在或已删除 |
| `VIDEO_NOT_PUBLISHED` | 404 | 视频未发布 | 视频存在但状态非 PUBLISHED（对非作者） |
| `USER_NOT_FOUND` | 404 | 用户不存在 | user_id 无效 |
| `UNAUTHENTICATED` | 401 | 未认证 | token 缺失、过期、签名无效 |
| `FORBIDDEN` | 403 | 无权限 | 访问私密内容、作者操作他人视频 |
| `RATE_LIMITED` | 429 | 请求过频 | 超过频率限制 |
| `DUPLICATE_ACTION` | 409 | 重复操作 | 幂等请求已处理过 |
| `UPLOAD_URL_EXPIRED` | 400 | 上传 URL 过期 | 客户端使用过期预签名 URL 创建视频 |
| `INTERNAL` | 500 | 服务端错误 | 数据库异常、Kafka 发送失败等 |
| `DEPENDENCY_UNAVAILABLE` | 503 | 依赖不可用 | Redis/Kafka/MinIO 故障 |

### 5.3 错误码使用原则

- Feed 接口返回 `INVALID_CURSOR` 时，客户端应清空游标重新请求第一页
- `DUPLICATE_ACTION` 表示操作已生效，客户端无需重试
- 下游错误（如 Kafka 发送失败）向上冒泡为 `INTERNAL`，日志记录真实原因

### 5.4 参考实现：response 包（Go/Hertz）

Hertz 不提供业务级响应包装，按 §2.2 约定自行封装。成功不包 envelope，错误统一结构。以下代码可直接作为 M0 骨架。

```go
// internal/response/response.go
package response

import (
    "github.com/cloudwego/hertz/pkg/app"
    "github.com/cloudwego/hertz/pkg/protocol/consts"
)

// BizError 对应 §5.1 的统一错误结构
type BizError struct {
    Code    string `json:"code"`
    Message string `json:"message"`
    Details any    `json:"details,omitempty"`
}

// --- 15 个错误码（对应 §5.2；实际以 internal/response 为准，含 §5.2 未列出的
// VIDEO_NOT_PUBLISHED / RATE_LIMITED / MISSING_AUTH_HEADER / INVALID_AUTH_TYPE /
// INVALID_TOKEN / DEPENDENCY_UNAVAILABLE / DUPLICATE_ACTION / FORBIDDEN /
// UPLOAD_URL_EXPIRED）---
var (
    ErrInvalidArgument       = &BizError{Code: "INVALID_ARGUMENT", Message: "invalid argument"}
    ErrInvalidCursor         = &BizError{Code: "INVALID_CURSOR", Message: "cursor has expired or is malformed"}
    ErrVideoNotFound         = &BizError{Code: "VIDEO_NOT_FOUND", Message: "video not found"}
    ErrVideoNotPublished     = &BizError{Code: "VIDEO_NOT_PUBLISHED", Message: "video not published"}
    ErrUserNotFound          = &BizError{Code: "USER_NOT_FOUND", Message: "user not found"}
    ErrUnauthenticated       = &BizError{Code: "UNAUTHENTICATED", Message: "invalid credentials"}
    ErrForbidden             = &BizError{Code: "FORBIDDEN", Message: "forbidden"}
    ErrRateLimited           = &BizError{Code: "RATE_LIMITED", Message: "too many requests"}
    ErrDuplicateAction       = &BizError{Code: "DUPLICATE_ACTION", Message: "duplicate action"}
    ErrUploadURLExpired      = &BizError{Code: "UPLOAD_URL_EXPIRED", Message: "upload url expired"}
    ErrInternal              = &BizError{Code: "INTERNAL", Message: "internal error"}
    ErrDependencyUnavailable = &BizError{Code: "DEPENDENCY_UNAVAILABLE", Message: "dependency unavailable"}
)

// --- code → HTTP 状态码映射（对应 §5.2）---
var statusByCode = map[string]int{
    "INVALID_ARGUMENT":        consts.StatusBadRequest,
    "INVALID_CURSOR":          consts.StatusBadRequest,
    "VIDEO_NOT_FOUND":         consts.StatusNotFound,
    "VIDEO_NOT_PUBLISHED":     consts.StatusNotFound,
    "USER_NOT_FOUND":          consts.StatusNotFound,
    "UNAUTHENTICATED":         consts.StatusUnauthorized,
    "FORBIDDEN":               consts.StatusForbidden,
    "RATE_LIMITED":            consts.StatusTooManyRequests,
    "DUPLICATE_ACTION":        consts.StatusConflict,
    "UPLOAD_URL_EXPIRED":      consts.StatusBadRequest,
    "INTERNAL":                consts.StatusInternalServerError,
    "DEPENDENCY_UNAVAILABLE":  consts.StatusServiceUnavailable,
}

// --- 成功响应：不包 envelope，直接输出（§2.2）---
func OK(ctx *app.RequestContext, data any) {
    ctx.JSON(consts.StatusOK, data)
}

func Created(ctx *app.RequestContext, data any) {
    ctx.JSON(consts.StatusCreated, data)
}

func Accepted(ctx *app.RequestContext, data any) {
    ctx.JSON(consts.StatusAccepted, data)
}

func NoContent(ctx *app.RequestContext) {
    ctx.SetStatusCode(consts.StatusNoContent)
}

// Error 输出统一错误结构（§5.1），HTTP 状态码由 code 映射
func Error(ctx *app.RequestContext, bizErr *BizError) {
    status := statusByCode[bizErr.Code]
    if status == 0 {
        status = consts.StatusInternalServerError
    }
    ctx.JSON(status, map[string]any{
        "error":      bizErr,
        "request_id": string(ctx.Response.Header.Get("X-Request-ID")),
    })
}
```

**Handler 用法示例**（Feed 接口）：

```go
// internal/feed/handler.go（摘录）
func (h *Handler) GetFeed(ctx context.Context, c *app.RequestContext) {
    userID, _ := c.GetInt64("user_id")
    limit := queryInt(c, "limit", 20)
    cursor := string(c.Query("cursor"))

    items, nextCursor, hasMore, err := h.svc.GetFeed(ctx, userID, cursor, limit)
    if err != nil {
        response.Error(c, response.ErrInvalidCursor)
        return
    }
    response.OK(c, FeedResponse{Items: items, NextCursor: nextCursor, HasMore: hasMore})
}
```

**panic 兜底**（recovery 中间件输出统一错误结构）：

```go
// cmd/api/main.go（摘录）
h := server.Default(server.WithHostPorts(":8080"))

h.Use(recovery.Recovery(
    recovery.WithRecoveryHandler(func(ctx context.Context, c *app.RequestContext, err any, stack []byte) {
        // 记录 stack 到日志后，输出统一错误结构
        response.Error(c, response.ErrInternal)
    }),
))
```

**X-Request-ID**：优先使用 hertz-contrib 生态的 `requestid` 中间件（生成 + 回传 header）；也可手写约 20 行（UUID v7 生成 + 注入 context + 写响应头）。二者与 `response.Error` 中的 `request_id` 读取逻辑兼容。

---

## 6. 缓存 Key 命名规范

### 6.1 Key 格式

```
项目:场景:版本:标识符[:限定]
```

| Key 模式 | 示例 | TTL |
|---|---|---|
| `feed:global:v1:first_page` | `feed:global:v1:first_page` | 30s + 随机 10s |
| `video:brief:{id}` | `video:brief:12345` | 60s + 随机 20s |
| `user:profile:{id}` | `user:profile:9` | 120s + 随机 30s |
| `feed:follow:{user_id}:page:{cursor_hash}` | `feed:follow:9:page:a1b2c3` | 15s |
| `video:like_count:{id}` | `video:like_count:12345` | 30s |
| `author:videos:{id}:last:N` | `author:videos:9:last:5` | 15s |
| `cursor:hmac:key` | `cursor:hmac:key` | 固定，不放缓存 |
| `follow:list:{user_id}` | `follow:list:9` | 60s |
| `rate_limit:{user_id}:{action}` | `rate_limit:9:feed` | 按策略 |

### 6.2 缓存策略矩阵

| 数据类型 | 模式 | 过期策略 | 重建策略 | 降级 |
|---|---|---|---|---|
| Feed 第一页 | 定时刷新 | TTL + 随机 | singleflight + DB | DB 直读，限流 |
| 视频摘要 | cache-aside | TTL + 随机 | singleflight + DB | DB 直读 |
| 关注列表 | cache-aside | TTL + 随机 | singleflight + DB | 短时间空列表返回空 Feed |
| 计数 | 异步更新 | 不主动过期 | Kafka 事件更新 | 返回上次缓存值 |

### 6.3 Redis 内存预算

- V1 预估数据量：10 万用户、5 万视频
- 每个视频摘要 ≈ 200 bytes → 10 MB
- Feed 第一页 ≈ 2 KB → 忽略
- 关注列表平均 500 人 × 8 bytes/ID → 4 KB/用户
- 活跃用户 1000 → 4 MB
- 总计 < 50 MB，V1 任意的 Redis 实例均可容纳

---

## 7. 数据库连接池配置（推荐初始值）

| 参数 | 推荐值 | 说明 |
|---|---|---|
| `MaxOpenConns` | 25 | 不超过 PostgreSQL `max_connections` 的 1/4 |
| `MaxIdleConns` | 10 | 保持空闲连接，减少建连开销 |
| `ConnMaxLifetime` | 30 min | 防止长时间连接被网络中间件断开 |
| `ConnMaxIdleTime` | 5 min | 释放长时间空闲连接 |

**启动后观察指标**：如果 `wait_count` 增长或 P99 升高，按 5 的步长上调 `MaxOpenConns`，同时观察 PostgreSQL 端 CPU。

---

## 8. 结构化日志字段标准

### 8.1 必选字段

| 字段 | 类型 | 示例 | 何时出现 |
|---|---|---|---|
| `request_id` | string | `req-abc123` | 所有请求日志 |
| `user_id` | int64 | `9` | 已认证请求 |
| `method` | string | `GET` | HTTP 日志 |
| `path` | string | `/api/v1/feed` | HTTP 日志 |
| `status` | int | `200` | HTTP 日志 |
| `latency_ms` | int | `45` | HTTP 日志 |
| `cursor_version` | string | `time-v1` | Feed 接口 |
| `cache_hit` | bool | `true` | 缓存相关操作 |
| `video_id` | int64 | `12345` | 视频相关操作 |
| `error_code` | string | `INVALID_CURSOR` | 错误响应 |

### 8.2 日志级别使用规则

| 级别 | 使用场景 |
|---|---|
| `DEBUG` | SQL 语句、缓存 key、开发调试信息 |
| `INFO` | 请求摘要、状态变更、非关键事件 |
| `WARN` | 降级生效、重试首次失败、游标校验失败 |
| `ERROR` | 数据库错误、Kafka 发送失败、panic |

### 8.3 脱敏规则

- 不记录 `password_hash`、`token` payload、`Authorization` header
- 日志中 `email` 显示为 `"li***@example.com"`
- 视频标题如果包含用户隐私内容，在日志中截断到 50 字符

### 8.4 迁移与 ORM 选型

**选型**：schema 管理用 [golang-migrate](https://github.com/golang-migrate/migrate)（库形式嵌入），运行时数据库访问用 GORM。

**为什么不用 AutoMigrate 管理 schema**：

| AutoMigrate 的问题 | 说明 |
|---|---|
| 只增不改 | 加字段可以，改类型/删字段/改约束不可靠 |
| 无版本概念 | 无法回答"数据库当前在哪个 schema 版本" |
| 不可审计 | 没有迁移历史记录，无法追溯 |

**分工原则**：

- `migrations/*.sql` 是 schema 的**唯一真相**：建表、改字段、索引全部以 SQL 迁移文件为准。
- GORM 模型是 schema 的**投影**：运行时查询/写入用 `struct`，但结构必须与 migration 产出的表一致。
- 禁止用 `db.AutoMigrate` 建表或改表；初始建表一律手写 `CREATE TABLE` 迁移。
- 连接池参数（`SetMaxOpenConns`、`SetConnMaxLifetime` 等）见 §7，GORM 通过底层 `*sql.DB` 同样生效。

**为什么 golang-migrate 用库而非 CLI**：项目目标是"新机器一条命令启动"（M0 完成定义），库形式可在启动时自动应用迁移，免去安装 CLI；CLI 形式留给需要多环境/回滚演练的阶段。

---

## 9. 关键 ADR（架构决策记录）

### ADR-001：选择 JWT 而非 Session（V1）

**上下文**：Feed v1 需要认证机制识别当前用户。

**候选方案**：
1. JWT Bearer Token（无状态）
2. Session + Redis Cookie（有状态）

**决策**：V1 选择 JWT。

**理由**：
- 减少架构复杂度：Redis 的引入应与缓存需求绑定，而非认证需求
- 接口测试友好：无需 cookie 管理
- Payload 可直接携带 user_id/role，避免 token 解码后额外查询

**代价**：
- Token 签发后无法主动失效（除非建黑名单）
- Payload 大小固定，不适合携带大量数据
- 密钥轮换需要优雅过渡

**V2 演进条件**：
- 需要实时封禁用户
- 需要检测 token 在多设备间被盗用
- 认证复杂度已证明值得引入 Redis 会话存储

### ADR-002：Feed 路由按场景分路径

**上下文**：Feed 有不同的获取场景——全局时间线、关注 Feed、作者主页。

**候选方案**：
1. 统一端点 + `type` 参数：`GET /api/v1/feed?type=global`
2. 按场景分路径：`GET /api/v1/feed`、`GET /api/v1/feed/following`、`GET /api/v1/users/{id}/videos`

**决策**：方案 2（分路径）。

**理由**：
- 路由层可直接对 `/feed` 和 `/feed/following` 配置不同的限流策略
- 中间件可差异化：`/feed/following` 必然要求认证，`/feed` 可匿名（可选）
- OpenAPI 文档更清晰：每个场景有独立的 path 和 schema

**代价**：
- URL 比方案 1 多出几个端点
- 但如果后续增加"热门 Feed"（`/feed/trending`）、"推荐 Feed"（`/feed/recommended`），扩展性优势更明显

### ADR-003：Feed 商品信息使用松耦合设计

**上下文**：Feed v1 为 Commerce 模块预留接口。Feed 列表中是否应该嵌入完整的商品信息。

**候选方案**：
1. 紧耦合：Feed item 直接嵌套 product 对象
2. 松耦合：Feed item 只返回 `product_ids` 数组

**决策**：方案 2（松耦合）。

**理由**：
- 当时 Feed v1 尚未引入 Commerce，松耦合有利于先独立开发和测试
- 商品详情有自己的缓存策略和错误处理，不应让 Feed 的响应延迟受商品查询影响
- 当前已按页批量装配 `product_cards`；本段仅保留早期方案比较

**代价**：
- 客户端需要额外一次请求获取商品详情
- 当前由 Feed/视频模块直接装配商品卡，未引入 API Gateway 或 BFF

### ADR-004：拉模式 + K=5 作为关注 Feed 默认策略

**上下文**：关注 Feed 需要展示用户关注作者的最新视频。

**候选方案**：
1. 推模式：发布时写入粉丝收件箱
2. 拉模式 + K=5：请求时实时查询

**决策**：V1 选择拉模式 + K=5。

**理由**：
- V1 用户规模小，拉模式足够支撑
- 推模式引入 Kafka fanout worker，复杂度高
- K=5 × 最多 1000 关注 = 最多 5000 条候选，内存归并可接受
- 关注列表缓存命中率高，进一步降低延迟

**代价**：
- 用户每次刷 Feed 都需要做数据库查询
- 关注数 > 1000 的用户 feed 加载较慢（可通过截断缓解）

**V2 演进条件**：
- 关注数超 1000 的用户占比 > 10%
- Feed 读取 P99 > 500ms
- 此时启动推拉混合实验

### ADR-005：先写数据库再删缓存

**上下文**：视频发布、删除、状态变更后，需要保证缓存最终与数据库一致。

**决策**：采用 Cache-Aside + 先写数据库再删缓存。

**理由**：
- 写数据库成功 → 删缓存 → 下次读重建，数据最终一致
- 如果使用先删缓存再写数据库，在并发读下会导致脏数据写入缓存
- 延迟双删（先删→写→再删）可进一步降低不一致窗口，但 V1 不引入——面试时能讨论即可

**代价**：
- 删缓存失败会导致短暂不一致，但 TTL 到期后自动修复
- 极端场景（删缓存失败且 TTL 很长）可通过消息队列补偿

### ADR-006：错误三层处理链

**上下文**：user 模块首个 repository 直接向调用方抛 `gorm.ErrRecordNotFound` 等 ORM 错误，service/handler 被迫 import gorm 才能判断错误类型。

**候选方案**：
1. 全链路共用 ORM 原始错误
2. 每层各自定义错误、层层翻译
3. repo 翻译成领域哨兵 → service 判定分类 → handler 映射 HTTP（选定）

**决策**：方案 3。

**理由**：
- 错误词汇表只有一份（领域哨兵），由 repo 的实现方从 ORM 错误翻译；接口属于消费者，哨兵与接口同包定义
- service 是唯一有业务上下文的地方，负责"已知/未知判定 + 日志分级 + 上下文包装"：已知业务失败（邮箱重复）不打 Error 日志，未知故障（死锁）才 slog.Error + `%w` 包装
- handler 无领域知识，只做哨兵 → BizError 的映射，未知错误兜底 500
- 判断规则：repo 只包装"service 需要 errors.Is 分支"的错误，其余原样上抛

**代价**：
- 每层多一层映射代码
- 错误类型数量必须克制：错误数量 ≈ 业务分支数量，否则退化成分类学

**V2 演进条件**：
- 微服务化后需引入 RPC 错误码协议（如 gRPC status），哨兵映射到协议错误码

### ADR-007：upload_tickets 防伪造上传凭证（V1）

**上下文**：创建视频时必须校验 object_key 是 upload-url 阶段发放的，防止客户端伪造 key 引用他人文件。

**候选方案**：
1. 客户端幂等 key + 内存/Redis 缓存已发放 URL
2. `upload_tickets` 表：upload-url 写入凭证，创建视频时条件核销（选定）
3. 仅校验 object_key 格式（videos/{user_id}/{uuid}.mp4）

**决策**：方案 2。

**理由**：
- 凭证必须可被多实例共享查询/核销，内存方案在多实例下失效；Redis 是 M4 的引入项，不为凭证提前引入
- 条件核销（`WHERE user_id=? AND object_key=? AND used=false AND expires_at>now()`）由数据库保证"一次性"，无需事务锁
- 校验顺序：StatObject（文件真实存在）→ ConsumeTicket（核销）→ CreateVideo（落库）。StatObject 失败不烧凭证，客户端可重试
- NoSuchKey 由 storage 层翻译为 `ErrObjectNotFound`（哨兵属于产生它的包），service 映射为 400 UPLOAD_URL_EXPIRED 而非 500

**代价**：
- 表会积累过期未用凭证，需定时清理（V1 未做，量级可忽略）
- 没有 Idempotency-Key：客户端重试会拿到新 URL，但不会产生脏数据

**V2 演进条件**：
- 引入 Redis 后可用 Idempotency-Key 语义（同键 5 分钟内返回同一 URL）
- 凭证清理任务随孤儿对象清理一起实现

### ADR-008：视频状态机的双层防护

**上下文**：视频状态转换（PENDING_REVIEW → PUBLISHED/DELETED 等）必须防并发竞态和非法迁移。

**候选方案**：
1. 应用层先查后改（Read-Modify-Write），无数据库约束
2. 表驱动状态机 + DB 条件更新（`WHERE id=? AND status=from`）（选定）

**决策**：方案 2。

**理由**：
- 状态机表集中定义合法迁移（`canTransition`），替换 if-else 瀑布，未来插入真实审核流程只改表不改结构
- DB 条件更新 + RowsAffected 判定：并发请求只有一个能命中前置状态，其余返回 ErrStatusConflict，竞态被数据库强制而非应用层约定
- 产品语义：作者可从任意非终态删除（含审核中），故 `PENDING_REVIEW → DELETED` 合法；该迁移由单测先行暴露缺失后补入状态机表
- 状态由自定义类型 `Status` 承载，编译期防拼错

**代价**：
- 每个迁移需要 from 状态，调用方必须知道当前状态（Delete 需先查）

**V2 演进条件**：
- 引入审核队列后增加 PROCESSING → PENDING_REVIEW 的真实异步迁移

### ADR-009：统一视频可见性策略

**上下文**：视频有 PUBLISHED/PENDING_REVIEW/DELETED 等状态，不同接口需要一致的回答"谁能看到什么"。

**候选方案**：
1. 各 handler 自行判断
2. service 集中实现策略，任何不可见一律 ErrNotFound（选定）

**决策**：方案 2。

**理由**：
- 不泄露存在性：未发布/已删除对无权限者返回 404 与"不存在"同语义，无法枚举
- DELETED 对所有人（含作者）不可见——"删除 = 不存在"；该语义由 E2E 抓到的 bug（删除后作者仍可见）驱动补全
- 策略只实现一次，Feed 查询、详情、将来的推荐系统复用同一语义
- 所有可见性检查集中在 service 的 Get 方法内，不散落 handler

**代价**：
- 接口语义统一为"拿不到就是不存在"，无法区分"未发布"和"已删除"（这是刻意的）

### ADR-010：测试分层策略与 E2E 的必要性

**上下文**：M1 期间 fake repo 单测全绿，但 E2E 真库验收抓出两个单测测不出的 bug：`column` 自定义类型拼接破坏 gorm SQL 生成（整条条件被当参数值绑定）；NoSuchKey 被分类为 500 而非 400；删除后作者仍可见。

**候选方案**：
1. 仅单测（fake repo / fake storage）
2. 单测 + 里程碑 E2E 验收（选定）

**决策**：方案 2。

**理由**：
- fake repo 测业务分支（错误映射、状态机、凭证校验），但不经过 SQL 生成、不经过真实错误分类（minio NoSuchKey）
- E2E 用真实 Postgres + MinIO 跑完整上传时序，验证协议边界语义：错误码分类、HTTP 状态、可见性
- 每个里程碑结束时跑一遍 E2E 是"写测试证明问题存在"工作方式的落地点
- 单测断言用 require.ErrorIs 而非 Equal，验证错误包装链而非表面相等

**代价**：
- E2E 依赖本地容器环境，无法进 CI 单测流水线（可后续用 Testcontainers 自动化）

**V2 演进条件**：
- 引入 CI 后可用 Testcontainers 把 E2E 自动化；新模块上线前必须过一遍 E2E 验收

### ADR-011：Refresh Token 黑名单 + 旋转（无状态 JWT 的可撤销性）

**上下文**：JWT 无状态（ADR-001）的代价是"签发后无法主动失效"。M4 前 login/register 签发 refresh token 但没有 /refresh 消费方，logout 不存在——token 泄露则 7 天内可无限使用。

**候选方案**：
1. 纯无状态（现状）：无法登出，泄露不可撤销
2. 黑名单只拉黑 refresh token（选定）
3. per-user 当前 jti 白名单（只认最新签发的一个）

**决策**：方案 2 + refresh token rotation；**access token 不进黑名单**。

**理由**：
- access 15min 短命，为它查 Redis 牺牲无状态 + 每个请求一次额外往返（feed 12k QPS 场景是纯开销）；工业标准是 access 短 TTL + refresh 可撤销
- 黑名单 key `blacklist:refresh:{jti}`，**TTL = token 剩余寿命**，到期自动消失——零清理任务，"TTL 即生命周期"
- `SET key val EX ttl NX` 单命令原子：NX 防止重复 Block 缩短已有更长的 TTL
- rotation 顺序：**先拉黑旧 jti 再签发新 token**——两步骤间崩溃的后果是旧 token 作废、用户重登，而不产生新旧双活（无事务下的安全优先排序）
- 幂等：logout 收到无效/过期/类型不符的 token 一律视为已登出（成功 no-op），只有 Redis 故障才报错
- 顺带修复：软删用户（000002）的 refresh token 必须作废——Refresh 时查用户存在性，ErrNotFound → 401

**代价**：
- 并发用同一 token 刷新时可能都通过检查各自签发（短暂双活窗口，TOCTOU）
- logout/refresh 依赖 Redis 可用性（Redis 挂则无法登出，只能等 TTL）

**V2 演进条件**：
- 需要多设备管理/强制下线时切换方案 3（白名单存 per-user 当前 jti，天然解决并发双活）

### ADR-012：Feed 首页缓存策略（Cache-Aside + singleflight + double-check）

**上下文**：100 万数据下无缓存 feed 6,576 QPS（7 行基线 12,219 是假象，索引深度随数据量增长），db_pool_wait_count 5.8 万，25 连接饱和。M4 引入 Redis 缓存读路径。

**候选方案**：
1. Redis ZSET 缓存整个已发布视频有序列表，分页在缓存层做
2. 只缓存"默认 limit + 无游标"的第一页 JSON（选定）
3. 每页独立 key 缓存

**决策**：方案 2，Cache-Aside + 先写库再删缓存（ADR-005）+ singleflight + singleflight 内二次检查（double-checked locking）。

**理由**：
- 流量长尾：首页 QPS 占绝大多数，深层翻页命中率低不值得缓存；ZSET 方案有 score 精度问题（`(published_at,id)` 复合键装不进 double）和详情批量回源（fanout）复杂度，记入 V2
- 缓存的是 **service 拼完 URL 的产物**（FeedResult），repo 层缓存不了——缓存代码必须在 service 之后
- 自定义 limit 与缓存内容绑定，混用会串数据，故只缓存默认 limit
- **singleflight 只合并时间上重叠的调用**：缓存刚过期瞬间仍有缝隙，压测抓出的 flaky 证明其存在——必须二次检查（等待期间别人可能已回填），这才是防击穿的完整形态
- 缓存故障（Redis 挂）**降级直连 DB**，缓存不是单点故障；Set 失败只影响命中率，不影响本次响应
- 失效走消费侧接口：video 定义 `FeedCacheInvalidator`，main 装配 feed 的实现——video 不知道 feed 存在（模块边界不破）
- 失效失败只 Warn 不回滚：DB 已写入，脏窗口由 TTL（60s）兜底
- 压测验证：1M 数据无缓存 6,576 QPS / 缓存后 11,337 QPS（1.72x），db_pool_wait 归零

**代价**：
- 深层翻页不缓存；生产真实命中率取决于首页流量占比（压测 99.999% 是 10s 全首页流量的假象）
- TTL 周期内（最多 60s）发布/删除后首页可能短暂滞后
- 缓存后新瓶颈是 Redis GET + JSON 反序列化（~11.3k QPS 封顶）

**V2 演进条件**：
- 首页流量占比下降 / 需要覆盖翻页时：ZSET 列表缓存 + 详情 fanout
- 命中路径直接返回预序列化 bytes，省掉每请求 Unmarshal

### ADR-013：数据库约束取舍原则（FK/CHECK 的边界）

**上下文**：对"数据库尽可能轻、尽可能少约束"诉求的裁定。两类"轻"含义相反：不加多余的（对）vs 尽量少约束（错）。

**决策**：**唯一约束不可删**（幂等的地基，M3 的 ON CONFLICT 依赖冲突目标）；**CHECK 保留**（写入时零成本，防不可能状态）；**FK 按表评估**，不搞一刀切。

**评估标准**（三个变量）：
1. 写热不热（子表 INSERT 对父表行加共享锁，热行上锁竞争是删 FK 的真实动机）
2. 孤儿行可否容忍（有无生命周期/对账任务消化）
3. 当前量级下成本是否可测量（不可测量则保留完整性保证）

**逐表结论**：
| 约束 | 判定 | 理由 |
|---|---|---|
| follows/likes PK、users 唯一键 | 留 | 幂等（ON CONFLICT） |
| CHECK 不自动关注 | 留 | 零成本防不可能状态 |
| videos.author_id、follows.* FK | 留 | 低写频，防孤儿 |
| likes.video_id FK | 留（观察） | 热写路径；翻转条件见下 |
| upload_tickets.user_id FK | 唯一可讨论删 | ticket 是瞬态凭证，过期即废，孤儿由 expiry 生命周期消化 |

**翻转条件**（触发时才删，删后必须有兜底）：
- likes 单表写入达每秒几千行、压测出现锁等待/死锁 → 删 FK + 应用层校验 + 对账任务（如定时清理孤儿 likes）
- 分库分表时 FK 天然失效，需提前断

**代价**：
- 保留 FK 在超高写入量级会有限制；当前量级（每秒个位数写）成本为 0
- 任何约束的删除必须是 ADR 决策（写明代价与兜底策略），不是审美偏好

---

## 10. 关注 Feed 拉模式查询逻辑（实现：窗口函数版）

> **实现状态**：已落地（M6）。下方伪代码是设计期"逐作者查询 + 内存归并"的简单版；
> 实际实现改为**一条窗口函数 SQL 完成"每作者 TOP K"**（`ListFollowingCandidates`，
> `ROW_NUMBER() OVER (PARTITION BY author_id ORDER BY published_at DESC, id DESC)`），
> 避免 N+1；游标下推到窗口内；排序交给 Ranker（M8），分页截断在 service。

```
func GetFollowingFeed(ctx, viewerID, cursor, limit) (items, nextCursor, hasMore) {
    // 1. 读关注列表（优先缓存）
    followees := getFollowList(ctx, viewerID) // 最长 1000 人

    // 2. 最多取 1000 个关注作者的最新 K 条
    K := 5
    candidates := make([]Video, 0, len(followees)*K)
    for _, followeeID := range followees {
        videos := getLatestVideos(ctx, followeeID, K)
        candidates = append(candidates, videos...)
    }

    // 3. 按 published_at DESC, id DESC 排序
    sort.Slice(candidates, sortByPublishedAtDesc)

    // 4. 游标截断（如果有游标，跳过游标之前的记录）
    if cursor != nil {
        candidates = filterAfterCursor(candidates, cursor)
    }

    // 5. 取 limit+1 条判断 has_more
    pageSize := min(limit, 20)
    if len(candidates) > pageSize {
        hasMore = true
        candidates = candidates[:pageSize]
    }

    // 6. 填充互动状态（is_liked、可见性过滤）
    items = enrichItems(ctx, viewerID, candidates)

    // 7. 生成 next_cursor
    nextCursor = buildCursor(items[len(items)-1])

    return items, nextCursor, hasMore
}
```

---

## 11. V1 不做、明确标记为 postponed 的事项

| 事项 | 计划版本 | 原因 |
|---|---|---|
| 关注 Feed（`/feed/following`） | V2 / M6 | V1 只做全局时间排序 |
| 热门/推荐排序 | V2 / M8 | V1 只用 `published_at` |
| 视频转码 | 不做（项目声明） | 非求职目标 |
| Session 认证 | V2 | 见 ADR-001 |
| Outbox 模式 | 后续阶段 4 候选 | Commerce v1 未实现 Kafka 或 Outbox |
| 计数聚合表（`video_stats`） | V2 | 已实现为实时 COUNT 查询（§3.7 实现版）；拆独立表等计数成为读瓶颈再做 |
| Feed 全量列表缓存（ZSET + fanout） | V2 | 见 ADR-012 |
| per-user 当前 jti 白名单 | V2 | 见 ADR-011 |
| 搜索（OpenSearch） | 项目第二阶段 | 独立子项目 |
| 拆 Kitex 服务 | 项目第二阶段 | 见主手册 |

---

## 12. 快速启动检查清单（编码前确认）

- [ ] 决定 JWT_SECRET 生成方式（V1 用 `openssl rand -hex 32`）
- [ ] 确认 PostgreSQL 版本（推荐 16）
- [ ] 确认 MinIO 版本（推荐 latest）
- [ ] 编写 docker-compose.yml
- [ ] 编写 .env.example（含所有配置项）
- [ ] 创建 migrations/ 目录和 000001_create_users.sql
- [ ] 创建统一的 errors 包（code + message + details）——直接使用 §5.4 的 response 包骨架
- [ ] 实现 JWT 中间件（解析 + 注入 context）——直接使用 §1.3 的中间件骨架
- [ ] 实现请求 ID 中间件（UUID v7）
- [ ] 实现结构化日志中间件

## 13. 限流（M7 实现记录）

### 13.1 方案

Redis 固定窗口计数：`INCR` + 首个请求 `EXPIRE`（lua 原子），计数超过配额拒绝。

```lua
local c = redis.call('INCR', KEYS[1])
if c == 1 then redis.call('EXPIRE', KEYS[1], ARGV[1]) end
return c
```

**为什么固定窗口而不是令牌桶**：单命令、可解释；令牌桶的"突发容忍"对学习项目过度。
已知代价：窗口边界有毛刺（59s 打满 + 1s 打满 = 两倍突发），演进条件=需要平滑时换令牌桶 lua。

### 13.2 规则（main 挂载，生产正确性修正版）

| 规则 | 维度 | 配额 | 挂载 | 场景 |
|---|---|---|---|---|
| auth-account | 账号 | 10/min | /auth 子组 | 防暴力破解——**爆破的维度是账号不是 IP**（NAT 下多人共享 IP，按 IP 限会误伤真实用户），解析 login body 的 identifier/email/username |
| auth-ip | IP | 100/min | /auth 子组 | 防单一来源洪峰（放宽版） |
| read | IP | 6000/min | g 组（全部公开读继承） | 只挡爬虫/异常洪峰，阈值远高于真实用户行为（含 NAT 多人共享 + 客户端预加载） |
| write | user | 100/10s | protected 组 | 上传/发布/点赞/关注（登录后按 user_id） |

- auth 双规则叠加（按账号精确 + 按 IP 洪峰防护），两道独立计数，账号 10/min 先触发
- auth 请求还继承 read（6000/min），不会误伤
- 部署前置：反向代理/CDN 后需处理 X-Forwarded-For（否则 ClientIP 拿到代理 IP，全员共享一个限流 key）

### 13.3 降级与错误

- Redis 故障 → **fail-open** 放行 + Warn 日志（限流失效比接口不可用好）；
  认证类场景（login）若要 fail-closed（拒绝所有请求防爆破）需另行配置，注释已标注
- 超限 → 429 `RATE_LIMITED`（response.Abort 按 statusByCode 映射——修复了
  Abort 硬编码 401 的既有 bug：auth 场景全是 401 掩盖了它）
- `RATE_LIMIT_ENABLED` 总开关：压测/调试时关闭（feed 压测 50 并发会被 read 限流挡）

### 13.4 验证

- 单测：允许/拒绝(429)/fail-open 降级/key 维度（user vs IP 回退）
- e2e：连续 login 11 次 → 前 10 次 401、第 11 次起 429；vegeta 360 req 打 feed →
  200×200 + 160×429（窗口边界精确）
