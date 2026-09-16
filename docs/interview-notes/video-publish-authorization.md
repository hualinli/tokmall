# 视频发布授权：把“状态机”变成真正的授权检查

## 这个模块解决什么问题

视频创建后处于 `PENDING_REVIEW`，只有视频作者才能调用
`POST /videos/{id}/publish` 将它发布为 `PUBLISHED`。

原实现虽然接收了 `userID`，但发布时没有使用它：service 直接按
`video_id + status` 更新。如果用户知道另一个人的视频 ID，就可能把对方的
待发布视频发布出去。这是典型的水平越权（Horizontal Privilege Escalation），
不是普通的状态机重复操作问题。

## 采用的方案

发布分成“读以分类、写以原子约束”两步：

1. service 读取视频，区分不存在、已删除、非作者和非法状态。
2. 对作者且状态允许的视频，调用 repository 做条件更新。
3. repository 的单条 `UPDATE` 同时带上视频 ID、作者 ID 和前置状态：

   ```sql
   UPDATE videos
   SET status = 'PUBLISHED', published_at = now()
   WHERE id = $1
     AND author_id = $2
     AND status = 'PENDING_REVIEW';
   ```

4. `RowsAffected = 0` 统一解释为状态竞争/条件不满足，返回
   `ErrStatusConflict`。service 的前置读取只是为了给 HTTP 层提供准确的错误
   语义，不能代替数据库条件更新。

## 对外错误语义

| 情况 | service 错误 | HTTP 语义 | 原因 |
|---|---|---|---|
| 视频 ID 不存在 | `ErrNotFound` | `404 VIDEO_NOT_FOUND` | 资源不存在 |
| 视频已软删除 | `ErrNotFound` | `404 VIDEO_NOT_FOUND` | `DELETED` 对所有人不可见 |
| 视频存在但调用者不是作者 | `ErrNotFound` | `404 VIDEO_NOT_FOUND` | 未发布内容只对作者可见，不能通过发布接口探测存在性 |
| 作者发布非 `PENDING_REVIEW` 视频 | `ErrStatusConflict` | `409 DUPLICATE_ACTION` | 状态机不允许该迁移，或动作已经完成 |
| 读取后状态被其他请求改变 | `ErrStatusConflict` | `409 DUPLICATE_ACTION` | 数据库 CAS 条件更新失败 |

详情、播放和发布接口都遵循原约定：非作者访问未发布视频、以及访问已删除
视频，都不能通过错误响应推断内容存在性，因此统一返回 `404`。作者对已发布
视频再次执行发布仍返回 `409`，因为这是作者已知资源上的非法状态迁移。

## 核心不变量

- 一个视频只能由其 `author_id` 对应的用户发布。
- 只有 `PENDING_REVIEW -> PUBLISHED` 这一条发布迁移有效。
- 发布成功必须设置 `published_at`，发布失败不能改变状态，也不能失效 Feed 缓存。
- 授权检查和状态检查最终必须落在同一条带条件的数据库更新上；任何只依赖
  “先查后写”的实现都不具备并发安全性。
- 并发发布最多一个请求成功；另一个请求看到条件不满足并返回冲突。

## 为什么不能只先查再更新

下面的实现存在典型 TOCTOU（检查与使用之间的时间窗口）：

```text
请求 A：SELECT，看到 PENDING_REVIEW
请求 B：SELECT，看到 PENDING_REVIEW
请求 A：UPDATE status = PUBLISHED
请求 B：UPDATE status = PUBLISHED  -- 可能错误地再次执行副作用
```

将 `author_id` 和 `status` 放进 `WHERE` 后，数据库会对每条更新做原子条件
判断。即使两个请求同时读到旧状态，只有第一个请求能让 `RowsAffected` 为 1。
作者 ID 当前没有业务更新入口，但把它写入条件仍是重要的防御性约束：未来若增加
管理操作、异步 Worker 或复用 repository，调用方遗漏授权检查时，数据库层不会
静默放行越权写入。

## 测试如何证明

- 作者成功：验证状态变为 `PUBLISHED`，并验证传入 repository 的 `author_id`
  是当前用户。
- 非作者失败：返回 `ErrNotFound`，视频保持 `PENDING_REVIEW`，且不会触发写入。
- 不存在/已删除：返回 `ErrNotFound`。
- 状态冲突：重复发布返回 `ErrStatusConflict`，不再次失效 Feed 缓存。
- 并发发布：两个 goroutine 同时发布同一视频，恰好一个成功、一个收到
  `ErrStatusConflict`。
- repository DryRun 检查生成的 SQL 确实包含 `author_id`、`status` 和 `id`
  条件，避免未来重构时把授权条件删掉。

## 替代方案与取舍

### 只在 service 层做作者判断

代码更少，但 repository 可能被其他入口直接调用，或者未来新增路径忘记复用
授权逻辑；同时“先查后写”无法保证并发状态正确。这里保留 service 的业务错误
分类，同时把关键约束下沉到 SQL。

### 先锁行再更新

可以使用 `SELECT ... FOR UPDATE` 保证读取和更新之间不被并发修改，但需要事务、
锁持有时间更长，且仍然应该保留 `author_id + status` 条件作为防御性不变量。当前
单字段状态迁移用条件 `UPDATE` 已足够简单可靠。

### 对非作者返回 403

这能让客户端区分“资源不存在”和“没有操作权限”，但会泄露未发布视频的存在性。
当前项目明确约定“未发布仅作者可见，不可见一律 `ErrNotFound`”，所以发布接口
选择 `404`。如果未来产品允许非作者看到待审核资源，再单独引入 `403` 并同步
更新所有可见性入口与 API 契约。

## 后续演进

- 若引入管理员审核，不能简单让客户端传 role；应由认证上下文提供可信角色，
  并为管理员路径单独定义审计日志和授权策略。
- 若发布后需要通知 Feed、搜索或推荐，先写数据库再通过 Outbox 发出
  `VideoPublished` 事件；不要在数据库事务内直接依赖 MQ。
- 若状态迁移变多，可以把迁移条件、操作者角色和审计记录抽成显式状态机表，
  但仍保留数据库条件更新防止并发覆盖。
- 发布与 MinIO 对象校验仍是独立问题；本模块只修复业务记录的作者授权和状态
  迁移，不假设对象已经被完整审核。

## 面试时的一句话

“发布接口不能只按视频 ID 更新。我在 service 预读用于区分 404 和 409，真正
写入使用 `video_id + author_id + from_status` 的条件更新；非作者对未发布视频
统一得到 404，避免存在性泄露。即使两个请求并发，或者未来出现绕过 service 的
写入口，也不会发布他人的视频或重复完成状态迁移。”
