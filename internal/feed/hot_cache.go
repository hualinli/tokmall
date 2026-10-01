package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// RedisHotFeed 是热榜的 Redis 实现：分钟桶写入 + 冻结窗口读取。
//
// 三层防护（对应同类项目 GCFeed 的重建击穿缺陷）：
//  1. 后台物化：`HotWindowWorker` 每分钟把窗口算好，读路径正常情况下只读不算；
//  2. singleflight：窗口缺失时合并并发回源，N 个请求只做一次 60-key 合并；
//  3. double-check：合并后重新确认窗口已存在，避免"合并出空窗口"时的重复劳动。
type RedisHotFeed struct {
	client *redis.Client
	stats  HotWindowStats
	group  singleflight.Group
	now    func() time.Time
}

func NewRedisHotFeed(client *redis.Client, stats HotWindowStats) *RedisHotFeed {
	if stats == nil {
		stats = nopHotStats{}
	}
	return &RedisHotFeed{client: client, stats: stats, now: time.Now}
}

// AddHotScore 累加一次热度到分钟桶。
//
// 失败不返回给调用方业务路径（service 只记日志）：热度是"锦上添花"的数据，
// 丢一次计分只会让榜单少一点分数，而让点赞失败是业务问题。
func (c *RedisHotFeed) AddHotScore(ctx context.Context, videoID int64, action string, at time.Time) error {
	delta := HotScoreForAction(action)
	if c == nil || c.client == nil || videoID <= 0 || delta == 0 {
		return nil
	}
	c.stats.HotScoreUpdate(action)
	key := hotMinuteKey(at)
	pipe := c.client.Pipeline()
	pipe.ZIncrBy(ctx, key, float64(delta), hotRankMember(videoID))
	// 每次写入都续期：桶的存活时间从最后一次互动算起，避免"桶在窗口内提前过期"。
	pipe.Expire(ctx, key, hotMinuteBucketTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("add hot score: %w", err)
	}
	return nil
}

// MaterializeWindow 把最近 60 个分钟桶合并成冻结窗口。
//
// 返回窗口内的条目数。注意 ZUNIONSTORE 在**所有桶都为空**时不会创建目标 key，
// 因此调用方必须能接受"窗口不存在"（读路径据此判断热榜暂不可用并降级）。
func (c *RedisHotFeed) MaterializeWindow(ctx context.Context, windowEnd time.Time) (int, error) {
	if c == nil || c.client == nil {
		return 0, ErrHotFeedUnavailable
	}
	windowEnd = windowEnd.UTC().Truncate(time.Minute)
	windowKey := hotWindowKey(windowEnd)

	if _, err := c.client.ZUnionStore(ctx, windowKey, &redis.ZStore{
		Keys:      hotMinuteKeys(windowEnd),
		Aggregate: "SUM",
	}).Result(); err != nil {
		return 0, fmt.Errorf("materialize hot window: %w", err)
	}

	pipe := c.client.Pipeline()
	// 净分 ≤ 0 的条目直接移除：点赞又取消不应在榜单上留下僵尸条目。
	pipe.ZRemRangeByScore(ctx, windowKey, "-inf", "0")
	// 硬上限：只保留头部。热榜只需要前 N 名，保留全量会让合并成本随视频总数增长。
	// 注意 ZREMRANGEBYRANK 的下标是升序，0 是最低分，所以要删掉"倒数第 max+1 名及以下"。
	pipe.ZRemRangeByRank(ctx, windowKey, 0, -int64(hotWindowMaxEntries)-1)
	pipe.Expire(ctx, windowKey, hotWindowTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("trim hot window: %w", err)
	}

	size, err := c.client.ZCard(ctx, windowKey).Result()
	if err != nil {
		return 0, fmt.Errorf("hot window size: %w", err)
	}
	c.stats.HotWindowSize(int(size))
	return int(size), nil
}

// Page 读取热榜的一页。cursor 为 nil 表示第一页。
//
// 返回的 entries 已按 (score DESC, videoID DESC) 排序，且严格位于游标之后——
// 即使窗口在这期间被重建（内容有细微变化），也不会重复返回已翻过的条目。
func (c *RedisHotFeed) Page(ctx context.Context, windowEnd time.Time, cursor *HotCursor, limit int) ([]HotEntry, error) {
	if c == nil || c.client == nil {
		return nil, ErrHotFeedUnavailable
	}
	if limit <= 0 {
		limit = hotFeedDefaultLimit
	}
	windowEnd = windowEnd.UTC().Truncate(time.Minute)
	windowKey := hotWindowKey(windowEnd)

	if err := c.ensureWindow(ctx, windowKey, windowEnd); err != nil {
		return nil, err
	}

	// 读窗口。分数用**闭区间**上界（Max = 游标分数），同分组由下面的
	// tie-break 过滤——如果用开区间（Max = "(score"），同一分数的其余条目
	// 会被整组跳过（分页直接漏数）。
	fetch := int64(limit + 1)
	maxScore := "+inf"
	if cursor != nil && cursor.VideoID > 0 {
		maxScore = fmt.Sprintf("%d", cursor.HotScore)
	}
	values, err := c.client.ZRevRangeByScoreWithScores(ctx, windowKey, &redis.ZRangeBy{
		Min: "-inf", Max: maxScore, Offset: 0, Count: fetch,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("read hot window: %w", err)
	}

	entries := make([]HotEntry, 0, limit)
	seen := make(map[int64]struct{}, limit+1)
	skipped := int64(0)
	for _, value := range values {
		entry, ok := hotEntryFromZ(value)
		if !ok {
			continue
		}
		// tie-break：同一分数下按 video_id 降序。游标记的是上一页最后一条的
		// (score, video_id)，因此同分但 id 更大（已经翻过）的条目要跳过。
		if cursor != nil && cursor.VideoID > 0 && entry.HotScore == cursor.HotScore && entry.VideoID >= cursor.VideoID {
			skipped++
			continue
		}
		entries = append(entries, entry)
		seen[entry.VideoID] = struct{}{}
		if len(entries) == limit {
			break
		}
	}

	// 同分组太大时上面那一次可能被"已翻过的同分条目"占满窗口，
	// 用 Offset 跳过它们再补一次（集合在单次请求内是冻结的，offset 稳定）。
	if skipped > 0 && len(entries) < limit {
		more, err := c.client.ZRevRangeByScoreWithScores(ctx, windowKey, &redis.ZRangeBy{
			Min: "-inf", Max: maxScore, Offset: skipped, Count: int64(limit - len(entries)),
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("read hot window page 2: %w", err)
		}
		for _, value := range more {
			entry, ok := hotEntryFromZ(value)
			if !ok {
				continue
			}
			if cursor != nil && cursor.VideoID > 0 && entry.HotScore == cursor.HotScore && entry.VideoID >= cursor.VideoID {
				continue
			}
			// Offset 作用在**未过滤**的序列上，因此第二段会重新读到第一段
			// 已经收集过的条目——用 seen 去重（集合在单次请求内冻结，不会漏）。
			if _, exists := seen[entry.VideoID]; exists {
				continue
			}
			seen[entry.VideoID] = struct{}{}
			entries = append(entries, entry)
			if len(entries) == limit {
				break
			}
		}
	}
	return entries, nil
}

// hotEntryFromZ 把 ZSET 成员翻译成热榜条目。
func hotEntryFromZ(value redis.Z) (HotEntry, bool) {
	member, ok := value.Member.(string)
	if !ok {
		return HotEntry{}, false
	}
	videoID, ok := hotRankVideoID(member)
	if !ok {
		return HotEntry{}, false
	}
	return HotEntry{VideoID: videoID, HotScore: int64(value.Score)}, true
}

// ensureWindow 保证窗口存在：singleflight 合并 + double-check。
func (c *RedisHotFeed) ensureWindow(ctx context.Context, windowKey string, windowEnd time.Time) error {
	exists, err := c.client.Exists(ctx, windowKey).Result()
	if err != nil {
		return fmt.Errorf("check hot window: %w", err)
	}
	if exists == 1 {
		c.stats.HotFeedSource("cache")
		return nil
	}

	// 合并并发回源：不继承首个请求的 ctx（否则首个调用者断连会让所有等待者一起失败），
	// 用独立超时——这是 TokMall 在页缓存上已经踩过的"故障放大"。
	_, err, _ = c.group.Do(windowKey, func() (any, error) {
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), singleflightTimeout)
		defer cancel()
		// double-check：可能已被同批的另一个执行体建好。
		if exists, err := c.client.Exists(checkCtx, windowKey).Result(); err == nil && exists == 1 {
			// double-check 命中：别的执行体已经建好窗口，本执行体无需做事。
			return nil, nil //nolint:nilnil // 见上方注释：无需做事不是错误
		}
		c.stats.HotWindowRebuild("on_demand")
		return nil, c.materialize(checkCtx, windowEnd)
	})
	if err != nil {
		return err
	}

	exists, err = c.client.Exists(ctx, windowKey).Result()
	if err != nil {
		return fmt.Errorf("recheck hot window: %w", err)
	}
	if exists == 0 {
		// 60 个桶全空：没有任何互动热度。这不是错误，而是"热榜暂时没有内容"，
		// 由调用方降级为时间流。
		return ErrHotFeedUnavailable
	}
	c.stats.HotFeedSource("rebuilt")
	return nil
}

func (c *RedisHotFeed) materialize(ctx context.Context, windowEnd time.Time) error {
	_, err := c.MaterializeWindow(ctx, windowEnd)
	return err
}

// CurrentWindowEnd 返回当前应使用的窗口（当前分钟）。
func (c *RedisHotFeed) CurrentWindowEnd() time.Time {
	return c.now().UTC().Truncate(time.Minute)
}

// HotWindowWorker 每分钟把当前窗口物化好，让读路径只读不算。
//
// 为什么值得单独起一个 worker：窗口重建是 60-key 合并，放在读路径上意味着
// 每个"窗口刚过期"的瞬间都要有一次慢请求（同类项目就是这么做的，
// 而且没有 singleflight，等于自造击穿）。后台物化把这次成本挪到低频侧。
type HotWindowWorker struct {
	hot      *RedisHotFeed
	interval time.Duration
}

func NewHotWindowWorker(hot *RedisHotFeed, interval time.Duration) *HotWindowWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &HotWindowWorker{hot: hot, interval: interval}
}

// Run 阻塞运行直到 ctx 取消。启动时先物化一次（避免冷启动第一分钟没有窗口）。
func (w *HotWindowWorker) Run(ctx context.Context) {
	if w == nil || w.hot == nil {
		return
	}
	w.materializeOnce(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.materializeOnce(ctx)
		}
	}
}

func (w *HotWindowWorker) materializeOnce(ctx context.Context) {
	windowEnd := w.hot.CurrentWindowEnd()
	size, err := w.hot.MaterializeWindow(ctx, windowEnd)
	if err != nil {
		slog.Warn("hot window materialization failed", "window_end", windowEnd, "err", err)
		return
	}
	w.hot.stats.HotWindowRebuild("scheduled")
	if size > 0 {
		slog.Debug("hot window materialized", "window_end", windowEnd, "entries", size)
	}
}

var _ = errors.Is
