package existence

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFilterHasNoFalseNegatives 是布隆过滤器唯一不能让步的性质：
// 加进去的元素必须永远返回 true。假阳性可以容忍，假阴性会把真实数据判成不存在。
func TestFilterHasNoFalseNegatives(t *testing.T) {
	filter := NewFilter(10_000, 0.01)
	for i := int64(1); i <= 10_000; i++ {
		filter.Add(uint64(i))
	}
	for i := int64(1); i <= 10_000; i++ {
		require.Truef(t, filter.Test(uint64(i)), "id=%d 被误判为不存在（假阴性）", i)
	}
}

// TestFilterFalsePositiveRateMatchesTheory 对照公式验证误判率：
//
//	m = -n·ln(p)/(ln2)², k = (m/n)·ln2
//
// 实测值应当与目标同量级（这里给 2 倍余量，避免哈希分布抖动导致 flaky），
// 太小说明参数算错（位数组过大浪费内存），太大说明哈希质量差。
func TestFilterFalsePositiveRateMatchesTheory(t *testing.T) {
	const (
		n     = 20_000
		fp    = 0.01
		probe = 100_000
	)
	filter := NewFilter(n, fp)
	for i := int64(1); i <= n; i++ {
		filter.Add(uint64(i))
	}

	// 理论参数核对
	expectedBits := uint64(math.Ceil(-float64(n) * math.Log(fp) / (math.Ln2 * math.Ln2)))
	require.InDelta(t, float64(expectedBits), float64(filter.Bits()), float64(filter.Bits())*0.05,
		"位数组长度应与公式一致（允许 64 位对齐带来的 5% 偏差）")
	expectedHashes := math.Round(float64(filter.Bits()) / float64(n) * math.Ln2)
	require.InDelta(t, expectedHashes, float64(filter.Hashes()), 1)

	falsePositives := 0
	for i := int64(n + 1); i <= n+probe; i++ {
		if filter.Test(uint64(i)) {
			falsePositives++
		}
	}
	rate := float64(falsePositives) / float64(probe)
	t.Logf("误判率实测 %.4f%%（目标 %.2f%%，位数 %d，哈希数 %d）", rate*100, fp*100, filter.Bits(), filter.Hashes())
	require.Lessf(t, rate, fp*2, "误判率显著高于设计值，说明参数推导或哈希实现有问题")
}

// 真实读取在第二、第三轮重建及失败后仍须保留；不能只验证一次交换。
func TestRotatingRebuildKeepsLiveData(t *testing.T) {
	r := NewRotating(1000, .01)
	load := func(add func(int64) error) error { return add(42) }
	require.NoError(t, r.Rebuild(load))
	for range 3 {
		require.NoError(t, r.Rebuild(func(add func(int64) error) error {
			require.True(t, r.Test(42), "加载下一份数据前，旧数据仍可见")
			return add(42)
		}))
		require.True(t, r.Test(42))
		count, _, _ := r.Snapshot()
		require.Equal(t, uint64(1), count, "完整数据必须在 active，不在将被清空的缓冲")
	}
	before := r.Builds()
	require.Error(t, r.Rebuild(func(add func(int64) error) error {
		require.NoError(t, add(99))
		r.Add(777)
		return errors.New("loader failed")
	}))
	require.True(t, r.Test(42))
	require.True(t, r.Test(777), "失败不能丢失本地新增")
	require.Equal(t, before, r.Builds())
}

func TestRotatingRebuildPreservesConcurrentAdds(t *testing.T) {
	r := NewRotating(10000, .001)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.Rebuild(func(add func(int64) error) error {
			close(entered)
			<-release
			return add(1)
		})
	}()
	<-entered
	var wg sync.WaitGroup
	for i := uint64(2); i <= 1000; i++ {
		wg.Add(1)
		go func(id uint64) { defer wg.Done(); r.Add(id) }(i)
	}
	wg.Wait()
	close(release)
	require.NoError(t, <-done)
	for i := uint64(1); i <= 1000; i++ {
		require.True(t, r.Test(i), "id=%d", i)
	}
}

func TestRotatingRebuildDropsRemovedEntries(t *testing.T) {
	r := NewRotating(1000, .001)
	require.NoError(t, r.Rebuild(func(add func(int64) error) error { return add(42) }))
	require.NoError(t, r.Rebuild(func(add func(int64) error) error { return add(99) }))
	require.False(t, r.Test(42), "重建不能一直向旧 active 累积")
	require.True(t, r.Test(99))
}

// TestGuardWarmupPassesEverything：空过滤器对任何 ID 都返回"不存在"，
// 如果启动后立刻用它判断，真实数据会被整片 404。warmup 期间必须全部放行。
func TestGuardWarmupPassesEverything(t *testing.T) {
	guard := NewGuard(map[string]uint64{"video": 100}, &fakeMarker{}, 0.01)
	ok, err := guard.MayExist(context.Background(), "video", 42)
	require.NoError(t, err)
	require.True(t, ok, "warmup 期间必须放行")
	require.False(t, guard.Ready("video"))
}

// Bloom 阴性只说明未装载过，必须继续查询共享负缓存与数据库。
func TestGuardBloomMissFailsOpen(t *testing.T) {
	marker := &fakeMarker{}
	guard := NewGuard(map[string]uint64{"video": 1000}, marker, 0.01, WithStats(NopStats{}))
	require.NoError(t, guard.Rebuild("video", func(add func(int64) error) error {
		for i := int64(1); i <= 100; i++ {
			if err := add(i); err != nil {
				return err
			}
		}
		return nil
	}))

	ok, err := guard.MayExist(context.Background(), "video", 99)
	require.NoError(t, err)
	require.True(t, ok, "过滤器里的 ID 必须放行到下一层")

	checksBefore := marker.checks
	ok, err = guard.MayExist(context.Background(), "video", 999_999)
	require.NoError(t, err)
	require.True(t, ok, "未装载 ID 必须放行到数据库")
	require.Equal(t, checksBefore+1, marker.checks)
}

// TestGuardNegativeCacheShortCircuits：过滤器放行但负缓存命中时同样不查库。
func TestGuardNegativeCacheShortCircuits(t *testing.T) {
	marker := &fakeMarker{}
	guard := NewGuard(map[string]uint64{"video": 1000}, marker, 0.01, WithTTL(time.Minute), WithJitter(0))
	require.NoError(t, guard.Rebuild("video", func(add func(int64) error) error { return nil }))

	// 直接放一个 ID 进过滤器（模拟假阳性：过滤器说可能存在）
	guard.Add("video", 777)
	ok, err := guard.MayExist(context.Background(), "video", 777)
	require.NoError(t, err)
	require.True(t, ok, "过滤器说可能存在 → 继续往下走")

	guard.MarkMissing(context.Background(), "video", 777)
	require.Equal(t, 1, marker.marked)

	ok, err = guard.MayExist(context.Background(), "video", 777)
	require.NoError(t, err)
	require.False(t, ok, "负缓存命中应直接拒绝")
}

// TestGuardFailsOpenWhenRedisBroken：负缓存故障时放行到数据库。
// 缓存故障绝不能把真实存在的数据判成不存在。
func TestGuardFailsOpenWhenRedisBroken(t *testing.T) {
	marker := &fakeMarker{err: context.DeadlineExceeded}
	guard := NewGuard(map[string]uint64{"video": 1000}, marker, 0.01)
	require.NoError(t, guard.Rebuild("video", func(add func(int64) error) error {
		return add(5)
	}))

	ok, err := guard.MayExist(context.Background(), "video", 5)
	require.NoError(t, err)
	require.True(t, ok, "Redis 故障必须 fail-open")
}

// TestGuardRejectsUnknownNamespace：装配错误必须显式报错，而不是静默放行/拒绝。
func TestGuardRejectsUnknownNamespace(t *testing.T) {
	guard := NewGuard(map[string]uint64{"video": 10}, &fakeMarker{}, 0.01)
	_, err := guard.MayExist(context.Background(), "order", 1)
	require.ErrorIs(t, err, ErrNoFilter)
}

// TestMarkMissingTTLHasJitter：抖动让同一批 key 不在同一秒集中过期（防雪崩）。
func TestMarkMissingTTLHasJitter(t *testing.T) {
	marker := &fakeMarker{}
	guard := NewGuard(map[string]uint64{"video": 10}, marker, 0.01,
		WithTTL(time.Minute), WithJitter(30*time.Second))
	require.NoError(t, guard.Rebuild("video", func(add func(int64) error) error { return nil }))

	seen := map[time.Duration]bool{}
	for i := int64(1); i <= 200; i++ {
		guard.MarkMissing(context.Background(), "video", i)
	}
	for _, ttl := range marker.ttls {
		seen[ttl] = true
	}
	require.Greater(t, len(seen), 10, "TTL 抖动应产生足够分散的过期时间")
	for ttl := range seen {
		require.GreaterOrEqual(t, ttl, time.Minute)
		require.Less(t, ttl, time.Minute+30*time.Second)
	}
}

type fakeMarker struct {
	missing map[string]bool
	marked  int
	checks  int
	ttls    []time.Duration
	err     error
}

func (f *fakeMarker) MarkMissing(_ context.Context, namespace string, id int64, ttl time.Duration) error {
	if f.err != nil {
		return f.err
	}
	if f.missing == nil {
		f.missing = map[string]bool{}
	}
	f.missing[key(namespace, id)] = true
	f.marked++
	f.ttls = append(f.ttls, ttl)
	return nil
}

func (f *fakeMarker) IsMissing(_ context.Context, namespace string, id int64) (bool, error) {
	f.checks++
	if f.err != nil {
		return false, f.err
	}
	return f.missing[key(namespace, id)], nil
}

func key(namespace string, id int64) string {
	return namespace + ":" + string(rune(id))
}

func TestGuardOtherInstanceCreatedEntityPasses(t *testing.T) {
	marker := &fakeMarker{}
	a := NewGuard(map[string]uint64{"video": 1000}, marker, .01)
	b := NewGuard(map[string]uint64{"video": 1000}, marker, .01)
	for _, g := range []*Guard{a, b} {
		require.NoError(t, g.Rebuild("video", func(add func(int64) error) error { return add(1) }))
	}
	a.Add("video", 42)
	ok, err := b.MayExist(context.Background(), "video", 42)
	require.NoError(t, err)
	require.True(t, ok, "另一实例没有该 ID 时应继续查数据库")
	b.MarkMissing(context.Background(), "video", 999)
	ok, err = b.MayExist(context.Background(), "video", 999)
	require.NoError(t, err)
	require.False(t, ok, "真实 DB miss 的负缓存仍然有效")
}
