package existence

import (
	"context"
	"math"
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

// TestRotatingKeepsOldEntriesVisibleDuringRebuild 重建期间不能出现假阴性：
// 新过滤器只装了重建那一刻的数据，旧数据必须仍能从 standby 命中。
func TestRotatingKeepsOldEntriesVisibleDuringRebuild(t *testing.T) {
	rotating := NewRotating(1000, 0.01)
	for i := int64(1); i <= 100; i++ {
		rotating.Add(uint64(i))
	}
	require.False(t, rotating.Ready(), "重建完成前必须处于 warmup")

	// 模拟重建：只装入后 50 个 ID
	rotating.BeginRebuild()
	for i := int64(51); i <= 100; i++ {
		rotating.Add(uint64(i))
	}
	rotating.CommitRebuild()
	require.True(t, rotating.Ready())

	for i := int64(1); i <= 100; i++ {
		require.Truef(t, rotating.Test(uint64(i)), "重建后 id=%d 变成不存在", i)
	}
	require.Equal(t, int64(1), rotating.Builds())
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

// TestGuardRejectsAbsentIDAfterLoad：装载完成后，不存在的 ID 被过滤器直接挡掉，
// 既不查 Redis 也不查库。
func TestGuardRejectsAbsentIDAfterLoad(t *testing.T) {
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
	require.False(t, ok, "不存在的 ID 应被布隆过滤器挡掉")
	require.Equal(t, checksBefore, marker.checks, "布隆过滤器拒绝时不应访问 Redis")
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
