# HotRanker 与游标分页：为什么当前不开放

## 结论

TokMall 当前生产关注 Feed 只启用 `TimeRanker`。它按
`(published_at DESC, id DESC)` 排序，数据库召回、下一页游标和 service 的排序顺序完全一致，
因此可以提供稳定的深分页。

`HotRanker` 仍保留在 `internal/feed` 中做排序规则单元实验，但暂不通过配置或 `Service`
暴露为在线 Feed。配置 `FEED_RANK_STRATEGY=hot` 或未知值会在启动配置阶段直接失败。

## 原实现的问题

关注 Feed 的 SQL 先按时间游标裁剪候选：

```text
WHERE (published_at, id) < (cursor.published_at, cursor.id)
```

然后 service 才按热度分数排序。于是上一页最后一条记录只代表“时间序列上的边界”，
却被当成了“热度序列上的边界”。例如一条发布时间较旧但播放量很高的视频，可能已经被
时间条件提前裁掉；下一页也可能重新出现热度顺序应该更早的记录。结果就是翻页漏项、重复，
并且随着数据变化越来越难复现。

`rank_version` 只能回答“客户端使用的是哪一版排序规则”，不能改变游标 payload 的含义。
即使把 `time-v1` 改成 `hot-v1`，游标里仍然只有 `(published_at,id)`，排序键不一致的问题
没有被解决。

## 为什么当前选择禁用

当前候选召回是“每个作者最近 K 条”，游标也被下推到窗口函数查询中；HotRanker 的分数还
依赖请求时刻 `now`。在不重做召回和游标协议的情况下，给游标换一个版本号只是伪修复。

因此代码层有两道边界：

- `config.Load` 只接受生产策略 `time`，`hot` 和未知策略 fail-fast；
- `HotRanker.SupportsCursorPagination()` 返回 false，Service 拒绝装配它的在线分页路径。

这保留了热度公式、计数聚合和排序单测的学习价值，同时不会把错误的深分页语义交给真实用户。

## 将来正确支持 HotRanker 的候选方案

### 1. Score cursor

让游标携带完整排序键，例如：

```text
(score, published_at, video_id, rank_version)
```

查询必须能够按同一排序键召回“游标之后”的候选。分数必须在翻页期间保持确定，不能每次
请求都用新的 `now` 重新计算；还要处理浮点精度、计数更新和分数相同的 tie-break。

### 2. Snapshot cursor

第一次请求冻结一个 `snapshot_at` 或 `window_end`，后续请求使用相同快照计算分数，并把快照
标识放进签名游标。这样热度衰减不会在翻页过程中改变排序。快照可以是短时 Redis/数据库
状态，也可以是游标中的稳定时间边界，但候选召回仍必须覆盖完整的快照候选集。

### 3. Materialized ranking

由定时任务或事件消费方预计算热榜，把稳定的 `(rank_score, video_id)` 写入 Redis ZSET、
物化表或分页快照。在线查询直接使用这个排序键，游标可以用 score + member 或快照 offset。
代价是额外的更新延迟、过期清理、重建和故障恢复逻辑。

## 面试时如何解释

> 我实现了 HotRanker 的热度公式和计数聚合，但审查时发现现有关注 Feed 的召回与游标是
> 发布时间键，而 HotRanker 的输出是热度键。`rank_version` 只能让旧游标失效，不能让
> 两种排序键一致，所以我没有伪装成可深分页的热榜：生产配置只允许 time，HotRanker
> 只保留单元实验。后续如果需要上线，会选择 score cursor + 冻结快照，或者用物化热榜
> 提供稳定排序键，再补相应的压测和故障恢复验证。

