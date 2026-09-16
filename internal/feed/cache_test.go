package feed

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type fakeFollowingRedisClient struct {
	mu      sync.Mutex
	values  map[string]string
	delKeys [][]string
	delErr  error
}

func (f *fakeFollowingRedisClient) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx, "GET", key)
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(value)
	return cmd
}

func (f *fakeFollowingRedisClient) Set(ctx context.Context, key string, value interface{}, _ time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx, "SET", key, value)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[key] = fmt.Sprint(value)
	cmd.SetVal("OK")
	return cmd
}

func (f *fakeFollowingRedisClient) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx, "DEL", keys)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delKeys = append(f.delKeys, append([]string(nil), keys...))
	if f.delErr != nil {
		cmd.SetErr(f.delErr)
		return cmd
	}
	var deleted int64
	for _, key := range keys {
		if _, ok := f.values[key]; ok {
			delete(f.values, key)
			deleted++
		}
	}
	cmd.SetVal(deleted)
	return cmd
}

func TestJitteredTTL_Range(t *testing.T) {
	base := 60 * time.Second
	for i := 0; i < 1000; i++ {
		got := jitteredTTL(base)
		require.GreaterOrEqual(t, got, base-jitterWindow, "TTL 不低于 base-window")
		require.LessOrEqual(t, got, base+jitterWindow, "TTL 不高于 base+window")
	}
}

func TestJitteredTTL_Varies(t *testing.T) {
	// 抖动的意义是错开过期时间：若所有 Set 用同一 TTL，仍然同时过期（雪崩未解）。
	base := 60 * time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 2000; i++ {
		seen[jitteredTTL(base)] = true
	}
	require.Greater(t, len(seen), 1, "抖动应产生多个不同 TTL，否则雪崩防护无效")
}

func TestFollowingCacheKeysCoverAllSupportedRankVersions(t *testing.T) {
	keys := followingCacheKeys(7)
	require.ElementsMatch(t, []string{
		followingCacheKey(7, TimeRanker{}.Version()),
		followingCacheKey(7, HotRanker{}.Version()),
	}, keys)
}

func TestRedisFollowingFeedCacheInvalidateDeletesOnlyTargetUserAllVersions(t *testing.T) {
	client := &fakeFollowingRedisClient{values: map[string]string{
		followingCacheKey(7, TimeRanker{}.Version()): "time",
		followingCacheKey(7, HotRanker{}.Version()):  "hot",
		followingCacheKey(8, TimeRanker{}.Version()): "other-user",
	}}
	cache := &RedisFollowingFeedCache{client: client, ttl: time.Minute}

	require.NoError(t, cache.InvalidateFollowingFeed(context.Background(), 7))

	client.mu.Lock()
	defer client.mu.Unlock()
	require.NotContains(t, client.values, followingCacheKey(7, TimeRanker{}.Version()))
	require.NotContains(t, client.values, followingCacheKey(7, HotRanker{}.Version()))
	require.Equal(t, "other-user", client.values[followingCacheKey(8, TimeRanker{}.Version())])
	require.Equal(t, [][]string{followingCacheKeys(7)}, client.delKeys)
}

func TestRedisFollowingFeedCacheNilSafe(t *testing.T) {
	var cache *RedisFollowingFeedCache
	require.NoError(t, cache.InvalidateFollowingFeed(context.Background(), 7))
	require.NoError(t, cache.Set(context.Background(), 7, CurrentRankVersion, &FeedResult{}))
	result, err := cache.Get(context.Background(), 7, CurrentRankVersion)
	require.NoError(t, err)
	require.Nil(t, result)

	cache = &RedisFollowingFeedCache{}
	require.NoError(t, cache.InvalidateFollowingFeed(context.Background(), 7))
	require.NoError(t, cache.Set(context.Background(), 7, CurrentRankVersion, &FeedResult{}))
	result, err = cache.Get(context.Background(), 7, CurrentRankVersion)
	require.NoError(t, err)
	require.Nil(t, result)
}

func TestRedisFollowingFeedCacheInvalidateReturnsRedisError(t *testing.T) {
	redisErr := fmt.Errorf("redis unavailable")
	client := &fakeFollowingRedisClient{values: map[string]string{}, delErr: redisErr}
	cache := &RedisFollowingFeedCache{client: client, ttl: time.Minute}
	require.ErrorIs(t, cache.InvalidateFollowingFeed(context.Background(), 7), redisErr)
}
