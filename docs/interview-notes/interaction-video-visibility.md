# 互动接口的视频可见性

## 解决的问题

点赞和取消点赞现在只接受“对互动可见”的视频：视频记录存在且 `status = PUBLISHED`。不存在、未发布、已删除统一返回 `ErrVideoNotFound`，HTTP 层映射为 `404 VIDEO_NOT_FOUND`，不让调用方通过不同响应探测视频是否存在或当前状态。

这个规则同时覆盖 `POST /videos/{id}/like` 和 `DELETE /videos/{id}/like`。正常视频保留原来的幂等语义：重复点赞仍成功并返回 `liked=true`，重复取消仍成功并返回 `liked=false`。

## 为什么外键不够

`likes.video_id -> videos.id` 的外键只能保证引用的行存在，不能表达视频当前是否已经发布。视频是状态机：`PENDING_REVIEW`、`BLOCKED` 或 `DELETED` 的记录仍然存在，因而仍然可以通过外键插入点赞。若只依赖 FK，隐藏视频可能继续积累互动关系，甚至在统计查询中被算入计数。

因此 interaction 模块保留自己的最小 read model：repository 查询 `videos(id, status)`，只把 `PUBLISHED` 视为可互动；它不导入 video 模块的 `Video` model 或状态类型。状态事实仍归 video 模块所有，interaction 只消费自己需要的投影条件。

## 分层与错误语义

service 负责业务决策：先调用 `VideoIsInteractable`，把 false 统一翻译成 `ErrVideoNotFound`。repository 负责数据库访问和错误传递。handler 只把领域错误映射为统一的 `VIDEO_NOT_FOUND` 响应，不暴露“未发布”和“已删除”的区别。

写入 repository 还把 `status = 'PUBLISHED'` 放进 INSERT/DELETE 的 SQL 条件中。这样前置读检查之后，视频刚好被删除或下架时，写入侧仍有数据库条件兜底；对于 `ON CONFLICT DO NOTHING` 的重复点赞，只有在确认视频仍发布后才继续返回幂等成功。

## 并发与 TOCTOU 边界

视频状态检查和点赞写入不是一个跨模块状态机事务：

```text
service 读取 PUBLISHED
        ↓
repository 条件写入（再次要求 PUBLISHED）
        ↓
返回幂等结果
```

这不是把两个模块的领域事务耦合起来，而是把最终写入约束下推到同一个 PostgreSQL 数据库，缩小 check-then-write 窗口。如果视频在条件写入已经成功之后才被删除，那么这次点赞发生在视频仍为 `PUBLISHED` 的时刻；后续详情、Feed 和统计应各自继续按可见性过滤。

如果需要“视频删除后历史互动也立即物理清理”，可以在视频删除事务中级联清理或投递清理事件，但这会改变删除耗时和跨模块耦合，当前版本没有把它作为点赞接口成功的前置条件。关系表仍由 FK 保证视频删除时不会留下无法归属的引用，业务可见性则由状态谓词保证。

## 面试时怎么讲

面试官问“为什么不直接插 likes？”时，可以回答：

> FK 只能证明视频行存在，不能证明它是已发布状态。interaction 不拥有 video 的状态机，所以定义了一个最小 read model 查询 `id + status`，在 service 统一把不存在、未发布、已删除映射成 404。为缩小读后写竞态，repository 的点赞写入也带 `EXISTS videos WHERE status = PUBLISHED` 条件；重复请求在视频仍可见时通过唯一键保持幂等。

面试官继续问“为什么不让 video service 暴露一个大对象”时，可以回答：

> 模块边界按消费侧接口定义。interaction 只需要一个布尔事实，不需要共享 video model，也不让 interaction 依赖 video 的内部状态机。未来拆服务时，这个接口可以替换成视频可见性 read model 或缓存，而不会把领域类型传播到其他模块。

## 当前取舍

- 仍然使用同步 PostgreSQL 查询，没有为了点赞路径引入 Redis 或 MQ；当前问题是正确性和不可见性，不是可测量的写吞吐瓶颈。
- 先读再条件写是可解释的两层防线，但删除/下架与点赞之间没有跨领域分布式事务。若未来状态变更拆到独立服务，应改为带版本的可见性快照、事件驱动清理，或接受最终一致性并用对账任务收敛。
- 这项检查只约束互动接口；Feed、视频详情和点赞统计仍需由各自模块按自己的 read model 过滤，不能把 interaction 的查询当成全局可见性服务。
