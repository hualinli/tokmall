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
	// 带 rank_version：排序规则（time/hot）切换后缓存不串数据（M8 版本化与缓存的交叉点）。
	followingCachePrefix = "feed:following"
)

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
	client *redis.Client
	ttl    time.Duration
}

func NewRedisFollowingFeedCache(client *redis.Client, ttl time.Duration) *RedisFollowingFeedCache {
	return &RedisFollowingFeedCache{client: client, ttl: ttl}
}

func followingCacheKey(userID int64, rankVersion string) string {
	return fmt.Sprintf("%s:%d:%s", followingCachePrefix, userID, rankVersion)
}

func (c *RedisFollowingFeedCache) Get(ctx context.Context, userID int64, rankVersion string) (*FeedResult, error) {
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
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, followingCacheKey(userID, rankVersion), data, jitteredTTL(c.ttl)).Err()
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
