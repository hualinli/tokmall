# 软删除用户的互动可见性

## 这次收口解决什么问题

`users` 表同时保留了历史 `status` 列和软删除时间 `deleted_at`。当前用户域没有
独立的禁用业务流程，但 interaction 对“可被关注的用户”采用明确的最小规则：

```text
deleted_at IS NULL AND status = 'ACTIVE'
```

不存在、软删除、非 `ACTIVE` 的用户，对关注接口统一按不存在处理。关注/粉丝列表、
用户资料中的关注/粉丝计数，以及 `is_following` 个性化字段，也都不再返回或计算
这些用户。

覆盖的行为包括：

- `POST /users/{id}/follow` 和 `DELETE /users/{id}/follow` 的目标用户校验；
- `/users/{id}/following`、`/users/{id}/followers` 的列表资源校验与列表项过滤；
- interaction 的 `IsFollowingMany` 批量回填；
- user 模块的 `FollowCounts` 和 `IsFollowing` 只读查询。

## 为什么外键不够

`follows.follower_id` 和 `follows.followee_id` 的外键只保证对应的用户行存在，不能
表达该用户现在是否仍然可见、是否仍处于 `ACTIVE`。软删除只是给用户行写入
`deleted_at`，并不会触发普通外键删除；历史关注关系仍可能留在 `follows` 中。

如果列表或计数只查询 `follows`：

- 已删除用户会继续出现在关注/粉丝列表；
- 关注数会把不可见关系算进去；
- `is_following` 可能暴露对已删除目标的历史关系；
- 直接访问已删除用户的互动资源可能通过结果差异探测其存在。

所以 FK 是引用完整性约束，不是业务可见性约束。可见性必须成为每个读模型的
显式谓词。

## 跨模块 read model 的边界

interaction 不 import `internal/user` 的 `User` model。它只需要一个很小的事实：
目标用户是否满足 `deleted_at IS NULL AND status = 'ACTIVE'`，以及列表项所需的
公开资料字段。因此 repository 直接对共享的 `users` 表做 JOIN/存在性查询，形成
自己的 read model；user 模块仍然拥有用户领域模型和生命周期规则。

这种边界有两个好处：

1. interaction 不依赖 user 模块的内部类型和写入逻辑，将来拆服务时可以替换成
   用户可见性投影或缓存；
2. 每个消费方只读取自己需要的列和条件，避免把一个“用户对象”传播到所有模块。

关注列表的两种查询分别对列表对端做 active JOIN：`ListFollowing` 过滤 followee，
`ListFollowers` 过滤 follower。`IsFollowingMany` 同样 JOIN followee，防止已删除
目标被批量回填为 true。user 模块的两个计数子查询分别 JOIN 对端用户，再统计
active 关系。

## 对外错误语义

目标用户不存在、软删除或非 `ACTIVE` 时，Follow/Unfollow 返回同一个
`ErrUserNotFound`，HTTP 层返回 `404 USER_NOT_FOUND`。列表资源在查询前也检查其
所属用户，已删除用户的 following/followers endpoint 同样返回 404。

这样调用方无法通过“软删除”和“从未存在”之间的错误差异探测用户状态。列表中某
个关系对端刚刚被删除时，则只是从 JOIN 结果中消失，不返回一个带有半残资料的项。

## 一致性边界

关注接口采用 service 存在性检查用于错误分类，真正的关注 INSERT 通过
`INSERT ... SELECT ... WHERE EXISTS(active user)` 再次约束目标状态；repository
的列表、计数和状态查询也按 active 条件过滤。这是一个有意的两层边界：

```text
service：目标用户是否可关注、统一 404 语义
       ↓
repository：带 active EXISTS 的原子 INSERT
       ↓
所有读模型：只读 active 用户关系
```

这避免了“service 检查通过、写入前目标已被禁用”的普通 TOCTOU 窗口。用户状态
仍可能在 INSERT 提交后立即改变，此时历史关系可以暂时保留，但不会再通过列表、
计数或 `is_following` 对外可见；后续物理清理可异步收敛。当前阶段不把清理旧关系
作为关注请求成功的前置条件，也不把用户软删除扩展成 interaction 的事务。

`status` 的约束在历史迁移中已从数据库移出，用户域当前也没有完整的禁用流程；本次
只消费已经存在的 `status = 'ACTIVE'` 语义，没有新增状态或修改认证中间件。未来若
禁用状态获得独立业务定义，应由用户域统一发布可见性规则，再同步各模块 read model。

## 物理清理与过滤的取舍

现在优先过滤而不是在软删除时同步删除全部 follows，原因是：

- 软删除请求路径短且可重试，不需要把所有关系批量删除放进用户事务；
- 保留关系便于审计、恢复和后续数据对账；
- 查询通过 JOIN 条件立即满足“对外不可见”。

代价是 `follows` 可能积累历史关系，计数查询多一次 JOIN。若规模达到可测量的
瓶颈，再增加按批次运行的清理任务或归档表，并以唯一的清理批次/指标保证可观测、
可恢复；不能把物理清理当成当前可见性正确性的唯一防线。

## 面试时怎么讲

面试官问“用户软删了，关注关系怎么办？”时，可以回答：

> 外键只能保证用户行存在，不能保证用户仍可见。我们把 `deleted_at IS NULL`
> 和 `status = ACTIVE` 作为 interaction 的最小用户 read model：Follow/Unfollow
> 目标校验、列表 JOIN、`IsFollowingMany` 和 user 的计数查询都使用同一规则。
> 已删除用户统一按 404 处理，关系即使暂时保留也不会被列表、计数或个性化字段
> 暴露。物理清理是后续可观测的异步收敛，不承担实时可见性正确性。

继续追问“为什么不直接删 follows？”时：

> 同步级联清理会增加软删除耗时和失败面，也会让审计/恢复更困难。当前读量和
> 关系量不构成瓶颈，所以用显式 JOIN 过滤保证业务语义；达到数据规模后再做
> 分批归档或清理，并用对账验证过滤结果和清理结果一致。

## 如何验证

本模块用两类测试证明规则：

- service fake：已删除目标不能 Follow/Unfollow，列表排除已删除关系，正常关注仍
  保持幂等；user service fake 验证计数忽略已删除 follower/followee；
- SQL DryRun：验证 interaction 的 UserExists、following/followers、批量
  `IsFollowingMany`，以及 user 的 FollowCounts/IsFollowing 都包含 active 用户
  条件和必要 JOIN。

这些测试关注的是“查询不会忘记可见性谓词”，而不是把跨模块数据复制到另一套
model 中。
