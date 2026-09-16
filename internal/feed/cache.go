package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	firstPageKey = "feed:first_page"
	// followingCachePrefix 关注 Feed 第一页缓存前缀，完整 key：feed:following:{user_id}:{rank_version}。
	// 带 rank_version：未来排序版本变化时缓存不串数据；当前生产只使用 time-v1。
	followingCachePrefix = "feed:following"
)

// followingCacheVersionsForInvalidation 是关注 Feed 缓存失效时要覆盖的版本集合。
// hot-v1 虽不是生产可配置版本，仍保留在列表中以清理历史实验缓存；
// 关注缓存 key 按 rank_version 隔离，因此失效时必须覆盖这里登记的每个版本；
// 使用固定 key 列表而不是 SCAN，避免按前缀遍历 Redis，也避免误删其他用户的 key。
// 新增可对外使用的关注排序版本时，必须同步加入这里并补缓存失效测试。
func followingCacheVersionsForInvalidation() []string {
	return []string{TimeRanker{}.Version(), HotRanker{}.Version()}
}

// FirstPageCache feed 消费侧接口：缓存的是 service 拼完 URL 的产物
// （FeedResult），所以 Get/Set 操作的是完整结果而不是 model。
type FirstPageCache interface {
	Get(ctx context.Context) (*FeedResult, error)
	Set(ctx context.Context, result *FeedResult) error
	Del(ctx context.Context) error
}

// FollowingFeedCache 关注 Feed 第一页缓存（个性化：key 带 user_id 与 rank_version）。
// 与 FirstPageCache 同模式：缓存拼完 URL 的 FeedResult。
type FollowingFeedCache interface {
	Get(ctx context.Context, userID int64, rankVersion string) (*FeedResult, error)
	Set(ctx context.Context, userID int64, rankVersion string, result *FeedResult) error
}

// followingRedisClient 是 RedisFollowingFeedCache 实际需要的最小 Redis 能力。
// 用消费侧最小接口保留可测试性；生产中的 *redis.Client 自动满足它。
type followingRedisClient interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// CacheStats 缓存命中统计：装配方（main）用 prometheus counter 实现，
// 压测时观察命中率验证缓存效果。
type CacheStats interface {
	Hit()
	Miss()
}

// RedisFeedCache 首页缓存的 Redis 实现。
type RedisFeedCache struct {
	client *redis.Client
	ttl    time.Duration
}

func NewRedisFeedCache(client *redis.Client, ttl time.Duration) *RedisFeedCache {
	return &RedisFeedCache{client: client, ttl: ttl}
}

// jitterWindow 缓存 TTL 随机抖动幅度。TTL 固定会导致大量 key 同一时刻过期
// （雪崩：所有用户同时 miss → 同时打 DB）。抖动把过期时间在
// [base-window, base+window] 内错开，DB 压力平摊。
// 关键：必须在每次 Set 时独立抖动——构造时抖动一次会让所有 key 用同一个值，
// 仍然同时过期，抖动失去意义。
const jitterWindow = 20 * time.Second

// jitteredTTL 返回抖动后的 TTL。rand 全局函数在 Go 1.20+ 自动播种且并发安全。
func jitteredTTL(base time.Duration) time.Duration {
	offset := time.Duration(rand.Int63n(int64(2*jitterWindow))) - jitterWindow
	return base + offset
}

// RedisFollowingFeedCache 关注 Feed 第一页缓存的 Redis 实现（个性化 key）。
type RedisFollowingFeedCache struct {
	client followingRedisClient
	ttl    time.Duration
}

func NewRedisFollowingFeedCache(client *redis.Client, ttl time.Duration) *RedisFollowingFeedCache {
	return &RedisFollowingFeedCache{client: client, ttl: ttl}
}

func followingCacheKey(userID int64, rankVersion string) string {
	return fmt.Sprintf("%s:%d:%s", followingCachePrefix, userID, rankVersion)
}

func followingCacheKeys(userID int64) []string {
	versions := followingCacheVersionsForInvalidation()
	keys := make([]string, 0, len(versions))
	for _, version := range versions {
		keys = append(keys, followingCacheKey(userID, version))
	}
	return keys
}

func (c *RedisFollowingFeedCache) Get(ctx context.Context, userID int64, rankVersion string) (*FeedResult, error) {
	if c == nil || c.client == nil {
		return nil, nil
	}
	data, err := c.client.Get(ctx, followingCacheKey(userID, rankVersion)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := &FeedResult{}
	if err := json.Unmarshal(data, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *RedisFollowingFeedCache) Set(ctx context.Context, userID int64, rankVersion string, result *FeedResult) error {
	if c == nil || c.client == nil {
		return nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, followingCacheKey(userID, rankVersion), data, jitteredTTL(c.ttl)).Err()
}

// InvalidateFollowingFeed 精确删除一个 follower 的关注 Feed 第一页缓存。
// 当前 key 以 rank_version 隔离，所以一次 DEL 显式覆盖所有当前支持版本；不使用
// SCAN，避免全库遍历和误删其他用户。缓存删除失败由调用方记录 Warn，业务写入
// 不回滚，TTL 负责最终收敛。
func (c *RedisFollowingFeedCache) InvalidateFollowingFeed(ctx context.Context, followerID int64) error {
	if c == nil || c.client == nil || followerID <= 0 {
		return nil
	}
	return c.client.Del(ctx, followingCacheKeys(followerID)...).Err()
}

func (c *RedisFeedCache) Get(ctx context.Context) (*FeedResult, error) {
	data, err := c.client.Get(ctx, firstPageKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := &FeedResult{}
	if err := json.Unmarshal(data, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *RedisFeedCache) Set(ctx context.Context, result *FeedResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, firstPageKey, data, jitteredTTL(c.ttl)).Err()
}

func (c *RedisFeedCache) Del(ctx context.Context) error {
	return c.client.Del(ctx, firstPageKey).Err()
}

// InvalidateFeedCache 实现 video 模块的消费侧接口（video 定义、main 装配）：
// video 只知道"发布/删除后要失效 feed 缓存"，不知道 feed 存在。
func (c *RedisFeedCache) InvalidateFeedCache(ctx context.Context) error {
	return c.Del(ctx)
}

// noopStats 默认空实现：没有埋点时不崩溃。
type noopStats struct{}

func (noopStats) Hit()  {}
func (noopStats) Miss() {}
