package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

	keys := append([]string{windowKey}, hotMinuteKeys(windowEnd)...)
	size, err := materializeHotWindow.Run(ctx, c.client, keys, hotWindowMaxEntries, int64(hotWindowTTL/time.Second)).Int()
	if err != nil {
		return 0, fmt.Errorf("materialize hot window: %w", err)
	}

	c.stats.HotWindowSize(int(size))
	return int(size), nil
}

// Page 读取热榜的一页。cursor 为 nil 表示第一页。
//
// 返回的 entries 已按 (score DESC, videoID DESC) 排序，且严格位于游标之后——
// 窗口存续期间不可变；旧游标引用的窗口过期时明确返回 ErrInvalidCursor。
func (c *RedisHotFeed) Page(ctx context.Context, windowEnd time.Time, cursor *HotCursor, limit int) ([]HotEntry, error) {
	if c == nil || c.client == nil {
		return nil, ErrHotFeedUnavailable
	}
	if limit <= 0 {
		limit = hotFeedDefaultLimit
	}
	windowEnd = windowEnd.UTC().Truncate(time.Minute)
	windowKey := hotWindowKey(windowEnd)

	if cursor != nil {
		exists, err := c.client.Exists(ctx, windowKey).Result()
		if err != nil {
			return nil, fmt.Errorf("check hot window: %w", err)
		}
		if exists == 0 {
			return nil, ErrInvalidCursor
		}
	} else if err := c.ensureWindow(ctx, windowKey, windowEnd); err != nil {
		return nil, err
	}

	maxScore, member := "+inf", ""
	if cursor != nil {
		maxScore = strconv.FormatInt(cursor.HotScore, 10)
		member = hotRankMember(cursor.VideoID)
	}
	values, err := readHotPage.Run(ctx, c.client, []string{windowKey}, maxScore, member, limit, hotWindowMaxEntries).Slice()
	if err != nil {
		return nil, fmt.Errorf("read hot window: %w", err)
	}
	entries := make([]HotEntry, 0, limit)
	for i := 0; i+1 < len(values); i += 2 {
		member, ok := values[i].(string)
		if !ok {
			return nil, errors.New("invalid hot window entry")
		}
		score, ok := values[i+1].(string)
		if !ok {
			return nil, errors.New("invalid hot window entry")
		}
		parsed, err := strconv.ParseFloat(score, 64)
		if err != nil {
			return nil, fmt.Errorf("parse hot score: %w", err)
		}
		entry, ok := hotEntryFromZ(redis.Z{Member: member, Score: parsed})
		if !ok {
			return nil, errors.New("invalid hot window entry")
		}
		entries = append(entries, entry)
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

// Both scripts run atomically: readers see a complete, immutable snapshot.
var materializeHotWindow = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return redis.call('ZCARD', KEYS[1]) end
redis.call('ZUNIONSTORE', KEYS[1], #KEYS - 1, unpack(KEYS, 2))
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', '0')
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -tonumber(ARGV[1]) - 1)
redis.call('EXPIRE', KEYS[1], ARGV[2])
return redis.call('ZCARD', KEYS[1])
`)

// Scan at most the bounded snapshot, returning only one page over the network.
// Zero-padded IDs preserve numeric ordering under Redis's lexicographic tie-break.
var readHotPage = redis.NewScript(`
local values = redis.call('ZREVRANGEBYSCORE', KEYS[1], ARGV[1], '-inf', 'WITHSCORES', 'LIMIT', 0, ARGV[4])
local result = {}
for i = 1, #values, 2 do
    if ARGV[2] == '' or tonumber(values[i+1]) < tonumber(ARGV[1]) or values[i] < ARGV[2] then
        result[#result+1] = values[i]
        result[#result+1] = values[i+1]
        if #result >= tonumber(ARGV[3]) * 2 then break end
    end
end
return result
`)
