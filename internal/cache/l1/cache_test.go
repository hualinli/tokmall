package l1

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCacheHitAndMiss(t *testing.T) {
	cache := New(4, 64, WithTTL(time.Minute))

	_, ok := cache.Get("k", 1)
	require.False(t, ok, "未写入必须未命中")

	cache.Set("k", []byte("v"), 1)
	value, ok := cache.Get("k", 1)
	require.True(t, ok)
	require.Equal(t, []byte("v"), value)
}

// TestVersionChangeInvalidatesLocally：版本号变化必须让本地副本立刻失效——
// 这是多实例场景下"别的实例写了新数据"的探测机制。
func TestVersionChangeInvalidatesLocally(t *testing.T) {
	cache := New(4, 64, WithTTL(time.Minute))
	cache.Set("feed", []byte("old"), 7)

	_, ok := cache.Get("feed", 8)
	require.False(t, ok, "版本不一致必须判为未命中")

	stats := cache.Stats()
	require.Equal(t, int64(1), stats.Stales)
	require.Zero(t, stats.Entries, "被判陈旧的条目应当被移除，避免反复判断")
}

func TestTTLExpiry(t *testing.T) {
	cache := New(2, 16, WithTTL(20*time.Millisecond))
	cache.Set("k", []byte("v"), 1)
	require.Equal(t, 1, cache.Len())

	time.Sleep(30 * time.Millisecond)
	_, ok := cache.Get("k", 1)
	require.False(t, ok, "过期必须未命中")
	require.Zero(t, cache.Len(), "过期条目应被惰性清除")
}

// TestLRUEviction：容量上限必须生效，且淘汰的是最久未使用的条目。
func TestLRUEviction(t *testing.T) {
	cache := New(1, 3, WithTTL(time.Minute))
	cache.Set("a", []byte("1"), 1)
	cache.Set("b", []byte("2"), 1)
	cache.Set("c", []byte("3"), 1)

	// 访问 a，让 b 成为最久未使用
	_, _ = cache.Get("a", 1)
	cache.Set("d", []byte("4"), 1)

	_, ok := cache.Get("b", 1)
	require.False(t, ok, "最久未使用的 b 应被淘汰")
	for _, key := range []string{"a", "c", "d"} {
		_, ok := cache.Get(key, 1)
		require.Truef(t, ok, "%s 应仍在缓存中", key)
	}
	require.Equal(t, int64(1), cache.Stats().Evictions)
}

func TestDeleteAndPurge(t *testing.T) {
	cache := New(4, 32, WithTTL(time.Minute))
	cache.Set("a", []byte("1"), 1)
	cache.Set("b", []byte("2"), 1)

	cache.Delete("a")
	_, ok := cache.Get("a", 1)
	require.False(t, ok)
	require.Equal(t, 1, cache.Len())

	cache.Purge()
	require.Zero(t, cache.Len())
}

// TestConcurrentAccess：并发读写不应 panic 或产生竞态（配合 -race 运行）。
func TestConcurrentAccess(t *testing.T) {
	cache := New(8, 256, WithTTL(time.Minute))
	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 200 {
				key := fmt.Sprintf("k%d", i%32)
				if worker%2 == 0 {
					cache.Set(key, []byte(strconv.Itoa(i)), int64(i%5))
				} else {
					_, _ = cache.Get(key, int64(i%5))
				}
			}
		}(worker)
	}
	wg.Wait()
	require.LessOrEqual(t, cache.Len(), 256)
}

// ---- 跨实例失效：用两个 L1 实例 + 一个共享的版本号来源模拟多实例 ----

// versionStore 模拟 Redis 里的版本号（真实实现见 feed.VersionStamp）。
type versionStore struct {
	mu      sync.Mutex
	version int64
	reads   int
}

func (s *versionStore) Current(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.version, nil
}

func (s *versionStore) Bump() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
}

// TestTwoInstancesSeeWriteThroughVersionStamp 是两级缓存一致性测试：
// 实例 A 写入并递增版本后，实例 B 的 L1 副本必须失效（而不是继续返回旧值）。
func TestTwoInstancesSeeWriteThroughVersionStamp(t *testing.T) {
	store := &versionStore{}
	instanceA := New(4, 64, WithTTL(time.Minute))
	instanceB := New(4, 64, WithTTL(time.Minute))

	version, err := store.Current(context.Background())
	require.NoError(t, err)
	instanceA.Set("feed:first_page", []byte("v1"), version)
	instanceB.Set("feed:first_page", []byte("v1"), version)

	// 实例 A 写入新数据并递增版本号（真实路径：发布/删除视频时）
	store.Bump()
	version, err = store.Current(context.Background())
	require.NoError(t, err)
	instanceA.Set("feed:first_page", []byte("v2"), version)
	instanceA.Delete("feed:first_page") // 写入方同时删掉自己的副本，避免多一次版本判断

	// 实例 B 没有收到任何广播，但它会用新版本号校验本地副本
	version, err = store.Current(context.Background())
	require.NoError(t, err)
	_, ok := instanceB.Get("feed:first_page", version)
	require.False(t, ok, "实例 B 的旧副本必须因为版本不一致而失效")
	require.Equal(t, int64(1), instanceB.Stats().Stales)
}

var _ = redis.Nil
