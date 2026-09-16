# 关注 Feed 缓存失效

## 这次收口解决什么问题

关注 Feed 的第一页是按 viewer 个性化的缓存：key 包含关注者（follower）的
`user_id` 和排序版本。用户 Follow/Unfollow 成功后，如果仍然直接命中旧缓存，
新关注作者的视频可能暂时看不到，或者已取消关注作者的视频继续出现。

这次把关注写路径和 Feed 缓存连接起来，但没有让 interaction 依赖 feed 模块：
interaction 只定义一个消费侧的最小接口，main 在装配时把 feed 的 Redis 实现注入
进去。

```text
Follow/Unfollow
      │
      ├─ 先写 follows（幂等）
      └─ 成功后通知 follower 的关注 Feed 第一页失效
                         │
                         └─ DEL feed:following:{follower_id}:{rank_version}
```

## 为什么失效的是 follower，而不是 followee

关注关系 `(follower_id, followee_id)` 改变的是 follower 看到的内容集合：

- Follow 后，follower 的 Feed 可能新增 followee 的视频；
- Unfollow 后，follower 的 Feed 应移除 followee 的视频；
- followee 自己的关注 Feed 没有发生变化。

因此只删除 `follower_id` 对应的缓存。不会按作者或全局前缀清空缓存，也不会误伤
其他用户的个性化第一页。

## Cache-Aside 的写入顺序

这里采用标准的“先写库、后删缓存”：

1. service 校验目标用户可关注；
2. repository 执行幂等的 Follow/Unfollow；
3. 数据库写入成功后，调用 `InvalidateFollowingFeed`；
4. 缓存失效失败只记 Warn，关注接口仍返回成功。

不能反过来先删缓存再写库。若删缓存成功但数据库写入失败，后续读请求会重新加载
旧关系，产生一次无谓的击穿；更严重的是，如果写库持续失败，缓存会不断被删除。
先写库让“成功的业务事实”先落地，缓存只是随后收敛的派生数据。

## 为什么删除所有排序版本

关注 Feed 的 key 仍然按 `rank_version` 隔离：

```text
feed:following:{user_id}:time-v1
feed:following:{user_id}:hot-v1
```

只删当前运行版本会留下另一个版本的旧第一页。用户切换排序策略、灰度流量或
旧实例仍在提供服务时，就可能重新命中脏缓存。

实现维护一个显式的当前支持版本列表，一次 Redis `DEL` 删除目标用户的所有版本：

- 不使用 `SCAN`，不需要遍历 Redis keyspace；
- 不会删除其他用户的 key；
- 删除不存在的 key 仍是幂等成功；
- 新增可对外使用的关注排序版本时，必须把版本加入列表并补测试。

这是当前版本的明确边界。未来排序版本很多、或者需要动态注册时，可以改成每个用户
一个 generation/version namespace：写关注关系时递增 generation，读写缓存 key
带 generation，从而无需删除旧 key；旧 key 再由 TTL 回收。但在目前只有两个版本
时，固定 key 列表更容易审计和验证。

## 失败窗口与为什么不回滚

“先写库后删缓存”仍有一个短暂失败窗口：

```text
数据库关注写入成功
        ↓
Redis DEL 失败/超时
        ↓
短时间内可能读到旧第一页
        ↓
缓存 TTL 到期后重新从数据库加载正确结果
```

缓存删除失败不能回滚已经提交的关注关系，原因是：

- Redis 和 PostgreSQL 不是同一个事务，回滚数据库也不能保证 Redis 一定恢复；
- 让用户看到“关注失败”但关系实际已写入，会造成更严重的重试和状态错觉；
- 缓存不是事实来源，正确性由 `follows` 表保证；
- TTL 提供最终收敛，Warn 日志为监控和排障留下证据。

如果将来业务要求更短的陈旧窗口，可以增加删除重试队列、Outbox 或 cache
generation；这些是可靠性增强，不应把 Redis 删除变成关注请求的同步成功条件。

## 模块边界与 nil-safe 降级

interaction 定义：

```go
type FollowingFeedInvalidator interface {
    InvalidateFollowingFeed(context.Context, int64) error
}
```

它只表达“让某个 follower 的第一页失效”，不暴露 Redis client、feed key 格式或
FeedResult。main 同时把同一个 `RedisFollowingFeedCache` 实例作为 feed 读写缓存和
interaction invalidator 注入。

当 `FEED_FOLLOWING_CACHE_ENABLED=false` 时，main 注入真正的 nil 接口。service
会跳过失效调用，Feed 直接查库；关注功能仍然可用，只损失缓存性能。Redis 实现本身
也对 nil receiver/client 做 no-op，避免测试或关闭路径因 typed nil 触发 panic。

## 幂等操作为什么也允许失效

Follow 使用唯一约束和 `ON CONFLICT DO NOTHING`，重复 Follow 仍返回成功；Unfollow
重复删除不存在的关系也返回成功。service 不要求 repository 额外返回“本次是否真的
改变了行”，而是在每次成功操作后删除同一个缓存 key。

重复 `DEL` 是幂等且便宜的，换取了更简单的接口和更小的遗漏风险：缓存实现不会因为
某个调用方没有正确解释 `RowsAffected` 而保留旧第一页。若未来删除成本成为可测量
瓶颈，再让 repository 返回 `changed` 并只在实际改变时失效。

## 测试如何证明

测试覆盖三层：

- service：Follow/Unfollow 成功后只通知 follower；重复操作仍成功并重复失效；
  缓存失效返回错误时业务写入仍成功；缓存关闭时 nil-safe；
- cache：一次 `DEL` 覆盖 `time-v1` 和 `hot-v1`，但不删除其他用户的 key；Redis
  错误向上返回给 service，由 service 降级处理；nil receiver/client 不 panic；
- main 装配：缓存开启时 feed cache 与 invalidator 是同一实例，关闭或 Redis client
  不可用时返回真正的 nil 接口。

## 面试时怎么讲

面试官问“关注关系变更后，Feed 缓存怎么保证不脏？”时，可以回答：

> 关注 Feed 是按 follower 和 rank version 隔离的第一页缓存。Follow/Unfollow
> 成功后，我采用先写 PostgreSQL、再精确删除这个 follower 的所有支持排序版本
> key；不做全局清缓存，也不使用 SCAN。删除失败只记录 Warn，不回滚已经成功的
> 关注关系，依靠 TTL 最终收敛，因为 Redis 只是派生缓存，事实来源是 follows 表。
> interaction 只依赖消费侧 invalidator 接口，由 main 注入 feed 实现，缓存关闭时
> 直接降级查库。

继续追问“为什么不把删缓存失败当错误？”时：

> PostgreSQL 和 Redis 没有共享事务，返回失败并不能证明关系没有写入，反而会让
> 客户端重试并制造重复操作。这个场景接受短暂 stale read；如果业务需要更短窗口，
> 可以用 Outbox 重试删除，或者用每用户 generation 让新读请求自动绕过旧 key。

继续追问“为什么要删 hot 版本？”时：

> key 按排序版本隔离，所以只删除当前实例的 time key 会留下 hot key 的旧第一页。
> 当前支持版本数量很少，我维护显式版本列表并用一次 DEL 覆盖它们；这样避免 SCAN，
> 也把新增排序版本时必须同步修改失效列表这个边界变成了可测试的代码约定。
