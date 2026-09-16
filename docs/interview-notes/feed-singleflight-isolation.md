# Feed 缓存与 singleflight 的请求隔离

## 这次收口解决什么问题

公开 Feed 和关注 Feed 的第一页都使用 cache-aside + singleflight：缓存未命中时，
多个同时到达的请求只查一次数据库，结果再由各请求继续补充实时字段。

问题在于 `singleflight.Group.Do` 会把同一个返回值交给所有等待者。缓存接口也不能
假设所有实现都像 Redis JSON 反序列化一样天然返回新对象；测试缓存、进程内缓存或
将来的替换实现可能直接返回同一个 `*FeedResult`。

`like_count` 不进入 Feed 缓存，而是在请求完成召回后实时批量查询。原实现虽然复制了
`Items` 后再写点赞数，但最后仍然给共享的 `FeedResult.Items` 字段赋值：多个请求会
并发写同一个外层结构，仍然存在数据竞争；如果缓存持有该指针，缓存快照也可能被
请求级字段污染。

## 所有权边界

```text
Redis/cache Get ───────┐
                       ├─> firstPage/followingFirstPage
singleflight Do ───────┘        │
                                │  可能是共享只读快照
                                ▼
                    List/ListFollowing: cloneFeedResult
                                │
                                │  每个请求独占 FeedResult + Items backing array
                                ▼
                    fillLikeCounts：原地写 like_count
                                │
                                ▼
                         返回请求结果
```

缓存和 singleflight 产出的基础结果只包含已经装配好的视频卡片、URL、游标和分页
信息，视为不可变快照。`firstPage` 与 `followingFirstPage` 不修改这个快照；它们把
可能共享的结果交给上层。只有 `List`/`ListFollowing` 确认走第一页缓存路径时，才
调用 `cloneFeedResult`，然后把独占结果交给 `fillLikeCounts`。

不走缓存的自定义 limit、游标翻页、作者主页，结果由当前请求刚刚创建，本来就拥有
自己的 `FeedResult` 和 `Items` backing array，因此不额外复制。这样既覆盖了共享
路径，也避免每个普通翻页请求都付一次复制成本。

## 为什么必须复制两层

只复制 `FeedResult` 外层是不够的：结构体里的 `Items` 是 slice header，复制结构体
仍然会指向同一个底层数组。请求 A 改 `Items[0].LikeCount` 时，请求 B 和缓存仍会
看到同一个元素。

`cloneFeedResult` 做两件事：

1. 复制 `FeedResult` 值，避免对 `Items`、`NextCursor`、`HasMore` 等字段的赋值影响
   共享外层对象；
2. 为 `Items` 分配新的 backing array 并复制元素，避免请求级字段写入共享数组。

`NextCursor` 指向的字符串也复制指针指向的字符串值。`FeedItem` 里的 URL 指针在
   service 内只读，点赞数更新不会触碰它们，所以没有为了每次请求递归复制字符串；
   这是本次 copy-on-read 的最小安全边界。

## 为什么不让 fillLikeCounts 自己复制

复制责任放在共享结果离开缓存/singleflight 的边界更清晰：

- `fillLikeCounts` 只接收调用方已经拥有的结果，并原地填写动态字段；
- 缓存快照不会在 service 的后续步骤中被意外修改；
- 不缓存的全新结果无需再复制；
- 以后增加其他请求级字段时，可以复用同一个所有权边界，而不是在每个字段更新
  点上猜测对象是否共享。

这不是深拷贝所有对象。复制成本是 O(n) 个 FeedItem，n 是一页的条数；当前默认
上限为 100，相比一次点赞聚合查询和 URL 装配，这个成本可控。若将来缓存结果变得
非常大，应先测量内存分配和延迟，再考虑 immutable DTO、按字段分离缓存或在缓存层
直接返回解码后的独立对象，不能为了省一次小切片复制而重新引入共享可变状态。

## 并发与缓存语义

公开 Feed 的流程是：

1. `firstPage` 先查缓存；未命中时进入 `singleflight`，只让一个调用查询视频并回填；
2. `List` 对 singleflight/缓存返回的基础快照做 copy-on-read；
3. 每个请求独立查询点赞数并写自己的 `Items`；
4. 缓存里保留没有请求级 `like_count` 的基础快照。

关注 Feed 完全采用同一个所有权规则，只是缓存键额外包含 `viewer_id` 和
`rank_version`。因此不同用户不会共享关注内容；同一用户的并发请求也不会共享
可变响应对象。

singleflight 只合并“同时发生的加载”，并不意味着返回值可以被调用方随意修改。它
解决的是击穿时的查询放大，不是自动提供对象不可变性；对象所有权仍需要由业务代码
明确管理。

## 如何验证

测试缓存故意直接返回进程内同一个 `*FeedResult`，以覆盖 Redis 反序列化天然隔离
可能掩盖的问题。并发测试让多个请求在 `LikeCounts` 处同时暂停，随后一起恢复，
稳定验证：

- 公开 Feed 和关注 Feed 的并发请求各自得到不同的 `FeedResult`；
- `Items` backing array 不共享；
- 缓存基础快照的 `like_count` 不会被请求污染；
- singleflight 仍只执行一次视频/候选查询；
- `go test -race ./internal/feed` 不报告数据竞争。

## 面试时怎么讲

面试官问“singleflight 返回同一个对象会不会有问题？”时，可以回答：

> 会。singleflight 只合并加载，不会复制返回值。我的 Feed 缓存结果是不可变基础
> 快照，`like_count` 这种请求阶段补充的动态字段不能直接写回它。第一页从缓存或
> singleflight 返回后，我在 service 边界复制 `FeedResult` 和 `Items` 底层数组，
> 再由请求独占的结果原地填充。新建的翻页结果本来就归当前请求，不额外复制。

继续追问“为什么浅拷贝结构体不够？”时：

> 因为 slice 复制的只是指针、长度和容量，底层数组仍然共享。必须复制 Items
> backing array；本次只更新 LikeCount，URL 指针在 service 内只读，所以不做无谓
> 的递归深拷贝。

再问“Redis 已经反序列化出新对象，为什么还要复制？”时：

> Redis 实现目前确实是 JSON 反序列化，但缓存接口不应该把这种实现细节当成所有权
> 保证。进程内缓存、测试 double 或未来替换实现可能直接返回共享对象。把缓存结果
> 当不可变快照，并在 service 的消费边界 copy-on-read，能让并发安全成为接口语义，
> 而不是某个缓存实现的偶然性质。

## 当前边界

- 本次只修复结果对象和 `Items` 数组的共享写入，不处理关注关系变化后的缓存失效。
- HotRanker 的热度排序与时间游标是否兼容是独立的分页正确性问题，本次不改变其
  行为。
- `FeedItem` 内的 URL 指针依赖 service 只读约定；若未来出现会修改 URL 指向内容
  的请求级逻辑，应扩展 clone 边界或改用值类型字段。
