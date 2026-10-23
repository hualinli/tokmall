package existence

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGuardNegativeCacheShortCircuits(t *testing.T) {
	marker := &fakeMarker{}
	guard := NewGuard([]string{"video"}, marker, WithTTL(time.Minute), WithJitter(0))

	ok, err := guard.MayExist(context.Background(), "video", 777)
	require.NoError(t, err)
	require.True(t, ok, "没有负缓存记录时必须放行到数据库")

	guard.MarkMissing(context.Background(), "video", 777)
	require.Equal(t, 1, marker.marked)

	ok, err = guard.MayExist(context.Background(), "video", 777)
	require.NoError(t, err)
	require.False(t, ok, "负缓存命中应直接拒绝")
}

func TestGuardFailsOpenWhenRedisBroken(t *testing.T) {
	marker := &fakeMarker{err: context.DeadlineExceeded}
	guard := NewGuard([]string{"video"}, marker)

	ok, err := guard.MayExist(context.Background(), "video", 5)
	require.NoError(t, err)
	require.True(t, ok, "Redis 故障必须 fail-open")
}

func TestGuardRejectsUnknownNamespace(t *testing.T) {
	guard := NewGuard([]string{"video"}, &fakeMarker{})
	_, err := guard.MayExist(context.Background(), "order", 1)
	require.ErrorIs(t, err, ErrNoFilter)
}

func TestMarkMissingTTLHasJitter(t *testing.T) {
	marker := &fakeMarker{}
	guard := NewGuard([]string{"video"}, marker, WithTTL(time.Minute), WithJitter(30*time.Second))

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
