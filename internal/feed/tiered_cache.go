package feed

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lihualin/tokmall/internal/cache/l1"
)

// feedVersionKey 是 Feed 缓存的**数据版本号**：任何会改变首页内容的写入
// （发布、删除、改标题）都要递增它。两级缓存靠它完成跨实例失效：
// 写入方递增版本，读方用版本校验自己的 L1 副本是否过期。
//
// 为什么不用 Pub/Sub 广播失效：广播需要每个实例维护订阅连接，
// 断线期间写入的失效消息会丢（消息不持久），恢复后本地副本可能长期陈旧；
// 版本号无需逐条广播，但失效写入和版本读取都可能失败。正常路径的
// 探测间隔（默认 1 秒）不是故障下的硬上界；L1 到期还可能读到旧 L2，
// 最终需依赖各层 TTL 收敛。
const feedVersionKey = "feed:version"

// VersionStamp 维护"当前数据版本"，并把读到的值在本地缓存一小段时间。
//
// 为什么要本地缓存版本：如果每次 L1 查询都去 Redis GET 一次版本号，
// L1 省下的就只剩反序列化，收益大打折扣。本地缓存把版本读取的
// 频率降到 1/refreshInterval；失效与版本读取成功时，探测带来最多一个
// 间隔的发现延迟。故障时会沿用旧版本，不能承诺这个上界。
type VersionStamp struct {
	client   *redis.Client
	interval time.Duration
	now      func() time.Time
	key      string

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
	value, err := s.client.Get(ctx, s.versionKey()).Int64()
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

// Live 读取 Redis 里的版本，不使用本地探测缓存。
// 回填要和这个值比较：本地缓存的版本可能还停在失效之前。
func (s *VersionStamp) Live(ctx context.Context) (int64, error) {
	value, err := s.client.Get(ctx, s.versionKey()).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return value, err
}

func (s *VersionStamp) versionKey() string {
	if s.key == "" {
		return feedVersionKey
	}
	return s.key
}

// Bump 递增版本号（写路径调用）。返回错误时调用方应当记录日志但**不能**
// 让写事务失败：缓存失效失败最多导致短暂的旧数据，而业务写入失败是资金问题。
func (s *VersionStamp) Bump(ctx context.Context) error {
	err := s.client.Incr(ctx, s.versionKey()).Err()
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
// L2 中的页面带着写入时的版本，和当前 Redis 版本不一致则当未命中。
// 写路径：写数据库后先 Delete（本地 L1）、再 Del（L2），成功后 Bump
// （版本号，让其它实例的 L1 失效）；L2 删除失败时不执行 Bump。
// 回源回填用回源前读到的版本做条件写入，版本已经变化就放弃这次回填。
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

	if versioned, ok := c.l2.(versionedFirstPage); ok {
		return c.getVersioned(ctx, versioned)
	}
	result, err := c.l2.Get(ctx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		// 两级都未命中：返回"没有缓存"而不是错误，调用方回源数据库。
		c.stats.Miss()
		return nil, nil //nolint:nilnil // 见上方注释：未命中不是错误
	}
	c.stats.Hit()
	c.fillL1(ctx, result)
	return result, nil
}

type versionedFirstPage interface {
	GetWithVersion(ctx context.Context) (int64, *FeedResult, error)
	SetIfVersion(ctx context.Context, observed int64, result *FeedResult) (bool, error)
}

func (c *TieredFeedCache) getVersioned(ctx context.Context, page versionedFirstPage) (*FeedResult, error) {
	stored, result, err := page.GetWithVersion(ctx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		c.stats.Miss()
		return nil, nil //nolint:nilnil // 未命中不是错误
	}
	live, err := c.liveVersion(ctx)
	if err != nil {
		// 版本读不到时不把这份页面写进 L1，避免标成一个猜出来的新版本。
		// 请求仍返回已经取出的 L2 页面：版本探测失败不应把首页打成 500。
		c.stats.Hit()
		return result, nil //nolint:nilerr // 见上方注释：版本读取失败不否决已取出的页面
	}
	if stored != live {
		c.stats.Miss()
		return nil, nil //nolint:nilnil // 版本不符按未命中，调用方回源
	}
	c.stats.Hit()
	c.fillL1At(result, live)
	return result, nil
}

// Observe 返回回源前应记下的版本。优先读 Redis，不用本地探测缓存。
func (c *TieredFeedCache) Observe(ctx context.Context) int64 {
	version, err := c.liveVersion(ctx)
	if err != nil && c.version != nil {
		return c.version.Current(ctx)
	}
	return version
}

// Fill 只在版本仍是回源前观察到的值时写入两级缓存。
// 版本已经变化说明中间发生过失效，这次查询结果不能覆盖新版本。
func (c *TieredFeedCache) Fill(ctx context.Context, result *FeedResult, observed int64) error {
	if page, ok := c.l2.(versionedFirstPage); ok {
		wrote, err := page.SetIfVersion(ctx, observed, result)
		if err != nil || !wrote {
			return err
		}
		c.fillL1At(result, observed)
		return nil
	}
	if c.version != nil && c.version.Current(ctx) != observed {
		return nil
	}
	if err := c.l2.Set(ctx, result); err != nil {
		return err
	}
	c.fillL1At(result, observed)
	return nil
}

func (c *TieredFeedCache) liveVersion(ctx context.Context) (int64, error) {
	if c.version == nil {
		return 0, nil
	}
	type liveReader interface {
		Live(context.Context) (int64, error)
	}
	if live, ok := c.version.(liveReader); ok {
		return live.Live(ctx)
	}
	return c.version.Current(ctx), nil
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
	version := int64(0)
	if c.version != nil {
		version = c.version.Current(ctx)
	}
	c.fillL1At(result, version)
}

func (c *TieredFeedCache) fillL1At(result *FeedResult, version int64) {
	if c.l1 == nil || result == nil {
		return
	}
	raw, err := encodeFeedResult(result)
	if err != nil {
		return
	}
	c.l1.Set(firstPageKey, raw, version)
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
