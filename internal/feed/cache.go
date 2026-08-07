package feed

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const firstPageKey = "feed:first_page"

// FirstPageCache feed 消费侧接口：缓存的是 service 拼完 URL 的产物
// （FeedResult），所以 Get/Set 操作的是完整结果而不是 model。
type FirstPageCache interface {
	Get(ctx context.Context) (*FeedResult, error)
	Set(ctx context.Context, result *FeedResult) error
	Del(ctx context.Context) error
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
	return c.client.Set(ctx, firstPageKey, data, c.ttl).Err()
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
