package feed

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/lihualin/tokmall/internal/cache/l1"
	"github.com/redis/go-redis/v9"
)

// feedVersionKey 是 Feed 缓存的**数据版本号**：任何会改变首页内容的写入
// （发布、删除、改标题）都要递增它。两级缓存靠它完成跨实例失效：
// 写入方递增版本，读方用版本校验自己的 L1 副本是否过期。
//
// 为什么不用 Pub/Sub 广播失效：广播需要每个实例维护订阅连接，
// 断线期间写入的失效消息会丢（消息不持久），恢复后本地副本可能长期陈旧；
// 版本号方案没有"丢消息"这个失败模式——最坏情况只是版本探测的刷新间隔
// （默认 1 秒）内读到旧值，而 L1 的 TTL 是最终兜底。
const feedVersionKey = "feed:version"

// VersionStamp 维护"当前数据版本"，并把读到的值在本地缓存一小段时间。
//
// 为什么要本地缓存版本：如果每次 L1 查询都去 Redis GET 一次版本号，
// L1 省下的就只剩反序列化，收益大打折扣。本地缓存把版本读取的
// 频率降到 1/refreshInterval，代价是最长 refreshInterval 的失效延迟——
// 这个取舍必须写清楚，而不是假装"实时一致"。
type VersionStamp struct {
	client   *redis.Client
	interval time.Duration
	now      func() time.Time

	mu        sync.Mutex
	version   int64
	fetchedAt time.Time
	// lastErr 记录最近一次读取失败，便于排障（读取失败时沿用旧版本，
	// 也就是"继续用可能过期的版本号"，宁可多命中也不打爆数据库）。
	lastErr error
}

func NewVersionStamp(client *redis.Client, interval time.Duration) *VersionStamp {
	if interval <= 0 {
		interval = time.Second
	}
	return &VersionStamp{client: client, interval: interval, now: time.Now}
}

// Current 返回当前数据版本。Redis 不可用时返回上一次成功读到的版本（不报错），
// 让缓存继续工作——这是缓存层一贯的 fail-open 策略。
func (s *VersionStamp) Current(ctx context.Context) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.fetchedAt.IsZero() && s.now().Sub(s.fetchedAt) < s.interval {
		return s.version
	}
	value, err := s.client.Get(ctx, feedVersionKey).Int64()
	switch {
	case errors.Is(err, redis.Nil):
		// 还没有任何写入：版本 0。
		s.version, s.fetchedAt, s.lastErr = 0, s.now(), nil
	case err != nil:
		s.lastErr = err
		// 读不到就沿用旧值，并把时间戳往后推，避免每个请求都重试 Redis。
		s.fetchedAt = s.now()
	default:
		s.version, s.fetchedAt, s.lastErr = value, s.now(), nil
	}
	return s.version
}

// Bump 递增版本号（写路径调用）。返回错误时调用方应当记录日志但**不能**
// 让写事务失败：缓存失效失败最多导致短暂的旧数据，而业务写入失败是资金问题。
func (s *VersionStamp) Bump(ctx context.Context) error {
	err := s.client.Incr(ctx, feedVersionKey).Err()
	s.mu.Lock()
	s.fetchedAt = time.Time{} // 下一次读取强制回源
	s.mu.Unlock()
	return err
}

// LastError 返回最近一次版本读取错误（供指标/健康检查）。
func (s *VersionStamp) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// VersionSource 是"数据版本"的消费侧接口：生产实现是 VersionStamp（读 Redis），
// 测试可以注入固定版本。定义在消费侧而不是直接依赖 *VersionStamp，
// 是为了让两级缓存的失效逻辑可以在没有 Redis 的情况下被完整验证。
type VersionSource interface {
	Current(ctx context.Context) int64
	Bump(ctx context.Context) error
}

// TieredFeedCache 是"L1 进程内 + L2 Redis"的两级首页缓存。
//
// 读路径：L1（内存，命中即返回）→ L2（Redis）→ 未命中（调用方回源数据库）。
// 写路径：写数据库后 Del（L2）+ 本地 Delete（L1）+ Bump（版本号，让其它实例的 L1 失效）。
//
// l2 用 FirstPageCache 接口而不是具体类型：既保持了"消费侧定义接口"的项目约定，
// 也让这一层可以用假实现做单元测试（Redis 只用于集成验证）。
type TieredFeedCache struct {
	l2      FirstPageCache
	l1      *l1.Cache
	version VersionSource
	stats   CacheStats
}

// TieredCacheOptions 用选项而不是长参数列表：缓存层次会继续演进（将来可能加
// 单飞、预热），选项让新增维度不需要改所有调用点。
type TieredCacheOptions struct {
	L2      FirstPageCache
	L1      *l1.Cache
	Version VersionSource
	Stats   CacheStats
}

func NewTieredFeedCache(opts TieredCacheOptions) *TieredFeedCache {
	stats := opts.Stats
	if stats == nil {
		stats = noopStats{}
	}
	return &TieredFeedCache{l2: opts.L2, l1: opts.L1, version: opts.Version, stats: stats}
}

// Get 依次查 L1、L2。L2 命中会回填 L1（write-back on read）。
func (c *TieredFeedCache) Get(ctx context.Context) (*FeedResult, error) {
	if c.l1 != nil {
		if raw, ok := c.l1.Get(firstPageKey, c.version.Current(ctx)); ok {
			result, err := decodeFeedResult(raw)
			if err == nil {
				c.stats.Hit()
				return result, nil
			}
			// 反序列化失败说明本地副本坏了：删掉它并按未命中继续。
			c.l1.Delete(firstPageKey)
		}
	}

	result, err := c.l2.Get(ctx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		c.stats.Miss()
		return nil, nil
	}
	c.stats.Hit()
	c.fillL1(ctx, result)
	return result, nil
}

func (c *TieredFeedCache) Set(ctx context.Context, result *FeedResult) error {
	if err := c.l2.Set(ctx, result); err != nil {
		return err
	}
	c.fillL1(ctx, result)
	return nil
}

func (c *TieredFeedCache) Del(ctx context.Context) error {
	if c.l1 != nil {
		c.l1.Delete(firstPageKey)
	}
	return c.l2.Del(ctx)
}

// InvalidateFeedCache 实现 video 模块的消费侧接口：删除两级缓存并递增版本号。
// 版本号递增是"通知其它实例"的唯一手段（它们会在下次读取时发现版本变了）。
func (c *TieredFeedCache) InvalidateFeedCache(ctx context.Context) error {
	if err := c.Del(ctx); err != nil {
		return err
	}
	if c.version != nil {
		return c.version.Bump(ctx)
	}
	return nil
}

func (c *TieredFeedCache) fillL1(ctx context.Context, result *FeedResult) {
	if c.l1 == nil {
		return
	}
	raw, err := encodeFeedResult(result)
	if err != nil {
		return
	}
	c.l1.Set(firstPageKey, raw, c.version.Current(ctx))
}

// L1Stats 暴露一级缓存的计数（测试与看板用）。
func (c *TieredFeedCache) L1Stats() l1.Stats {
	if c.l1 == nil {
		return l1.Stats{}
	}
	return c.l1.Stats()
}

func decodeFeedResult(raw []byte) (*FeedResult, error) {
	result := &FeedResult{}
	if err := json.Unmarshal(raw, result); err != nil {
		return nil, err
	}
	return result, nil
}

func encodeFeedResult(result *FeedResult) ([]byte, error) {
	return json.Marshal(result)
}
