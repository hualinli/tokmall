package feed

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/cache/l1"
	"github.com/lihualin/tokmall/internal/testsupport"
)

// 真实 Redis 上复现：回源前记下版本，失效递增版本后再回填，旧页面不得被读回。
func TestRedisFillRejectsPageObservedBeforeBump(t *testing.T) {
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: testsupport.RedisAddr(t)})
	t.Cleanup(func() { _ = client.Close() })
	suffix := time.Now().UnixNano()
	pageKey := "feed:test:first_page:" + strconv.FormatInt(suffix, 10)
	versionKey := "feed:test:version:" + strconv.FormatInt(suffix, 10)
	t.Cleanup(func() { _ = client.Del(ctx, pageKey, versionKey).Err() })
	require.NoError(t, client.Set(ctx, versionKey, 1, 0).Err())

	version := NewVersionStamp(client, time.Hour)
	version.key = versionKey
	pageCache := NewRedisFeedCache(client, time.Minute)
	pageCache.pageKey = pageKey
	pageCache.versionKey = versionKey
	cache := NewTieredFeedCache(TieredCacheOptions{
		L2:      pageCache,
		L1:      l1.New(4, 16, l1.WithTTL(time.Minute)),
		Version: version,
	})
	observed := cache.Observe(ctx)
	require.Equal(t, int64(1), observed)
	require.NoError(t, cache.InvalidateFeedCache(ctx))

	stale := &FeedResult{Items: []FeedItem{{VideoID: suffix}}}
	require.NoError(t, cache.Fill(ctx, stale, observed))
	got, err := cache.Get(ctx)
	require.NoError(t, err)
	require.Nil(t, got)

	freshObserved := cache.Observe(ctx)
	require.Equal(t, int64(2), freshObserved)
	require.NoError(t, cache.Fill(ctx, &FeedResult{Items: []FeedItem{{VideoID: suffix + 1}}}, freshObserved))
	got, err = cache.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, suffix+1, got.Items[0].VideoID)
}
