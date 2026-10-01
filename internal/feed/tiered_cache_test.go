package feed

import (
	"context"
	"testing"
	"time"

	"github.com/lihualin/tokmall/internal/cache/l1"
	"github.com/stretchr/testify/require"
)

// fakeFirstPageCache 是一个可观测的 L2，用来验证"L1 命中时不再访问 L2"。
type fakeFirstPageCache struct {
	result *FeedResult
	gets   int
	sets   int
	dels   int
}

func (f *fakeFirstPageCache) Get(context.Context) (*FeedResult, error) {
	f.gets++
	return f.result, nil
}

func (f *fakeFirstPageCache) Set(_ context.Context, result *FeedResult) error {
	f.sets++
	f.result = result
	return nil
}

func (f *fakeFirstPageCache) Del(context.Context) error {
	f.dels++
	f.result = nil
	return nil
}

// fakeVersionSource 是可注入的版本号（生产实现是 Redis 版本号 + 本地短缓存）。
type fakeVersionSource struct {
	version int64
	bumps   int
}

func (f *fakeVersionSource) Current(context.Context) int64 { return f.version }

func (f *fakeVersionSource) Bump(context.Context) error {
	f.bumps++
	f.version++
	return nil
}

func newTestTieredCache(t *testing.T, l2 FirstPageCache, version VersionSource, stats CacheStats) *TieredFeedCache {
	t.Helper()
	return NewTieredFeedCache(TieredCacheOptions{
		L2:      l2,
		L1:      l1.New(4, 64, l1.WithTTL(time.Minute)),
		Version: version,
		Stats:   stats,
	})
}

// TestTieredCacheL1HitAvoidsL2：L1 命中时不得访问 L2。
// 用 panicFirstPageCache 证明"没有被调用"——比"计数为 0"更强。
func TestTieredCacheL1HitAvoidsL2(t *testing.T) {
	cache := newTestTieredCache(t, panicFirstPageCache{}, &fakeVersionSource{version: 3}, noopStats{})
	cache.fillL1(context.Background(), &FeedResult{Items: []FeedItem{{VideoID: 42}}})

	got, err := cache.Get(context.Background())
	require.NoError(t, err, "L1 命中时不应触碰 L2（L2 被调用会 panic）")
	require.Len(t, got.Items, 1)
	require.Equal(t, int64(42), got.Items[0].VideoID)
	require.Equal(t, int64(1), cache.L1Stats().Hits)
}

// TestTieredCacheL2HitFillsL1：L2 命中后必须回填 L1，让下一次请求走内存。
func TestTieredCacheL2HitFillsL1(t *testing.T) {
	l2 := &fakeFirstPageCache{result: &FeedResult{Items: []FeedItem{{VideoID: 7}}}}
	stats := &countingStats{}
	cache := newTestTieredCache(t, l2, &fakeVersionSource{version: 1}, stats)

	got, err := cache.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(7), got.Items[0].VideoID)
	require.Equal(t, 1, l2.gets)
	require.Equal(t, 1, stats.hits)
	require.Equal(t, 1, cache.L1Stats().Entries, "L2 命中要回填 L1")

	// 第二次：L1 命中，L2 不再被访问
	_, err = cache.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, l2.gets, "第二次必须命中 L1，不再访问 L2")
	require.Equal(t, int64(1), cache.L1Stats().Hits, "L1 层命中一次（第一次是 L1 未命中）")
	require.Equal(t, 2, stats.hits, "对调用方来说两次都是缓存命中")
}

// TestTieredCacheMissReportsMiss：两级都未命中时上报 miss（调用方回源数据库）。
func TestTieredCacheMissReportsMiss(t *testing.T) {
	l2 := &fakeFirstPageCache{}
	stats := &countingStats{}
	cache := newTestTieredCache(t, l2, &fakeVersionSource{}, stats)

	result, err := cache.Get(context.Background())
	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, 1, stats.misses)
}

// TestInvalidateDropsBothTiersAndBumpsVersion：写路径必须同时
// 删 L2、删本地 L1、递增版本号（让其它实例的 L1 失效）。三者缺一不可。
func TestInvalidateDropsBothTiersAndBumpsVersion(t *testing.T) {
	l2 := &fakeFirstPageCache{result: &FeedResult{Items: []FeedItem{{VideoID: 1}}}}
	version := &fakeVersionSource{version: 5}
	cache := newTestTieredCache(t, l2, version, noopStats{})
	cache.fillL1(context.Background(), &FeedResult{Items: []FeedItem{{VideoID: 1}}})
	require.Equal(t, 1, cache.L1Stats().Entries)

	require.NoError(t, cache.InvalidateFeedCache(context.Background()))
	require.Equal(t, 1, l2.dels, "L2 必须被删")
	require.Zero(t, cache.L1Stats().Entries, "本地 L1 必须被删")
	require.Equal(t, 1, version.bumps, "必须递增版本号通知其它实例")
}

// TestSetWritesThroughToBothTiers：回源后的写入要同时落到两级。
func TestSetWritesThroughToBothTiers(t *testing.T) {
	l2 := &fakeFirstPageCache{}
	cache := newTestTieredCache(t, l2, &fakeVersionSource{version: 2}, noopStats{})

	require.NoError(t, cache.Set(context.Background(), &FeedResult{Items: []FeedItem{{VideoID: 9}}}))
	require.Equal(t, 1, l2.sets)
	require.Equal(t, 1, cache.L1Stats().Entries)
}

type countingStats struct {
	hits   int
	misses int
}

func (s *countingStats) Hit()  { s.hits++ }
func (s *countingStats) Miss() { s.misses++ }

// panicFirstPageCache 在被调用时 panic：用来证明某条路径**没有**访问 L2。
type panicFirstPageCache struct{}

func (panicFirstPageCache) Get(context.Context) (*FeedResult, error) {
	panic("L2 must not be touched on L1 hit")
}

func (panicFirstPageCache) Set(context.Context, *FeedResult) error {
	panic("L2 must not be touched on L1 hit")
}

func (panicFirstPageCache) Del(context.Context) error {
	panic("L2 must not be touched on L1 hit")
}

// TestVersionChangeInvalidatesL1AcrossInstances 是跨实例失效的核心用例：
// 实例 B 的 L1 副本在"别人递增了版本号"之后必须失效，即使它没有收到任何广播。
func TestVersionChangeInvalidatesL1AcrossInstances(t *testing.T) {
	shared := &fakeVersionSource{version: 1}
	l2B := &fakeFirstPageCache{result: &FeedResult{Items: []FeedItem{{VideoID: 100}}}}
	instanceB := newTestTieredCache(t, l2B, shared, noopStats{})
	instanceB.fillL1(context.Background(), &FeedResult{Items: []FeedItem{{VideoID: 1}}})

	// 实例 A 写入新数据：递增共享版本号（真实路径由 InvalidateFeedCache 完成）
	require.NoError(t, shared.Bump(context.Background()))

	// 实例 B 下一次读取：L1 因版本不一致失效，回落到 L2 拿到新数据
	got, err := instanceB.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(100), got.Items[0].VideoID, "版本变化后必须读到 L2 的新数据")
	require.Equal(t, int64(1), instanceB.L1Stats().Stales, "旧副本应被判为陈旧并移除")
}
