package breaker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock 让状态机的测试不依赖真实时间。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testConfig() Config {
	return Config{
		Name:             "test",
		FailureThreshold: 3,
		OpenDuration:     5 * time.Second,
		MaxOpenDuration:  30 * time.Second,
		HalfOpenProbes:   1,
	}
}

func TestClosedBreakerPassesCalls(t *testing.T) {
	clock := newFakeClock()
	b := New(testConfig(), WithClock(clock.Now))

	for range 2 {
		require.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }))
	}
	require.Equal(t, StateClosed, b.State())
}

// TestOpensAfterConsecutiveFailures：连续失败达到阈值就打开，之后调用被快速拒绝
// （函数体不再执行）——这正是熔断保护调用方的意义。
func TestOpensAfterConsecutiveFailures(t *testing.T) {
	clock := newFakeClock()
	b := New(testConfig(), WithClock(clock.Now))

	calls := 0
	failing := func(context.Context) error { calls++; return errors.New("downstream down") }
	for range 3 {
		require.Error(t, b.Do(context.Background(), failing))
	}
	require.Equal(t, StateOpen, b.State())
	require.Equal(t, 3, calls)

	// 打开后：直接返回 ErrOpen，且不再执行函数体
	err := b.Do(context.Background(), failing)
	require.ErrorIs(t, err, ErrOpen)
	require.Equal(t, 3, calls, "熔断打开时不得再调用下游")
}

// TestSuccessResetsConsecutiveFailures：成功会清零连续失败计数，
// 避免"偶发失败累积"误触发熔断。
func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	clock := newFakeClock()
	b := New(testConfig(), WithClock(clock.Now))

	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("x") }))
	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("x") }))
	require.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }))
	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("x") }))

	require.Equal(t, StateClosed, b.State(), "失败计数应在成功后清零")
}

// TestHalfOpenRecoversAfterCooldown：冷却期结束后进入 half-open，
// 探测成功则恢复；这是"自动恢复"的关键路径。
func TestHalfOpenRecoversAfterCooldown(t *testing.T) {
	clock := newFakeClock()
	b := New(testConfig(), WithClock(clock.Now))
	tripBreaker(t, b)

	clock.Advance(5 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())

	require.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }))
	require.Equal(t, StateClosed, b.State(), "探测成功应恢复")
	require.Equal(t, 0, b.Snapshot().OpenCount, "恢复后连续打开次数清零")
}

// TestHalfOpenFailureReopensWithBackoff：探测失败要重新打开，且冷却期翻倍
// （避免下游没恢复就被持续探测）。
func TestHalfOpenFailureReopensWithBackoff(t *testing.T) {
	clock := newFakeClock()
	b := New(testConfig(), WithClock(clock.Now))
	tripBreaker(t, b)

	clock.Advance(5 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())
	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("still down") }))
	require.Equal(t, StateOpen, b.State())

	// 第二次冷却期是 10s：5s 后仍是 open
	clock.Advance(5 * time.Second)
	require.Equal(t, StateOpen, b.State(), "第二次冷却期应退避到 10s")
	clock.Advance(5 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())
}

// TestHalfOpenLimitsConcurrentProbes：half-open 只放行少量探测，
// 其余快速失败——否则恢复瞬间的流量会把刚恢复的下游再次打垮。
func TestHalfOpenLimitsConcurrentProbes(t *testing.T) {
	clock := newFakeClock()
	cfg := testConfig()
	cfg.HalfOpenProbes = 1
	b := New(cfg, WithClock(clock.Now))
	tripBreaker(t, b)

	clock.Advance(5 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())

	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = b.Do(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	// 探测名额被占用时，第二个请求必须被拒绝
	require.ErrorIs(t, b.Do(context.Background(), func(context.Context) error {
		t.Fatal("不应执行第二个探测")
		return nil
	}), ErrOpen)

	close(release)
	require.Eventually(t, func() bool { return b.State() == StateClosed }, time.Second, 5*time.Millisecond)
}

// TestFailureRatioTrips：低频依赖用失败率阈值（配合最小样本数）。
func TestFailureRatioTrips(t *testing.T) {
	clock := newFakeClock()
	cfg := testConfig()
	cfg.FailureThreshold = 100 // 关掉连续失败路径，只看失败率
	cfg.MinRequests = 4
	cfg.FailureRatio = 0.5
	cfg.Window = time.Minute
	b := New(cfg, WithClock(clock.Now))

	require.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }))
	require.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }))
	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("x") }))
	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("x") }))

	require.Equal(t, StateOpen, b.State(), "4 个样本中 2 个失败（50%）应触发熔断")
}

// TestWindowExpiryDropsOldSamples：窗口外的旧样本不参与失败率计算。
func TestWindowExpiryDropsOldSamples(t *testing.T) {
	clock := newFakeClock()
	cfg := testConfig()
	cfg.FailureThreshold = 100
	cfg.MinRequests = 4
	cfg.FailureRatio = 0.5
	cfg.Window = 10 * time.Second
	b := New(cfg, WithClock(clock.Now))

	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("x") }))
	require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("x") }))
	clock.Advance(11 * time.Second)
	require.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }))
	require.NoError(t, b.Do(context.Background(), func(context.Context) error { return nil }))

	require.Equal(t, StateClosed, b.State(), "窗口已滚动，旧失败不应再计入")
}

// TestStorageDecoratorFailsFast：装饰器把熔断套在对象存储上，
// 打开后不再调用真实客户端。
func TestStorageDecoratorFailsFast(t *testing.T) {
	clock := newFakeClock()
	b := New(testConfig(), WithClock(clock.Now))
	downstream := &fakeStorage{err: errors.New("minio down")}
	storage := NewStorage(downstream, b)

	for range 3 {
		_, err := storage.PresignGetURL(context.Background(), "videos/1/a.mp4")
		require.Error(t, err)
	}
	require.True(t, storage.IsOpen())
	callsAfterTrip := downstream.calls

	_, err := storage.PresignGetURL(context.Background(), "videos/1/a.mp4")
	require.ErrorIs(t, err, ErrStorageUnavailable)
	require.Equal(t, callsAfterTrip, downstream.calls, "熔断后不得再调用 S3")

	// 下游恢复 → 冷却期后探测成功 → 恢复正常
	downstream.err = nil
	clock.Advance(5 * time.Second)
	url, err := storage.PresignGetURL(context.Background(), "videos/1/a.mp4")
	require.NoError(t, err)
	require.Equal(t, "signed://videos/1/a.mp4", url)
	require.False(t, storage.IsOpen())
}

func tripBreaker(t *testing.T, b *Breaker) {
	t.Helper()
	for range 3 {
		require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("down") }))
	}
	require.Equal(t, StateOpen, b.State())
}

type fakeStorage struct {
	err   error
	calls int
}

func (f *fakeStorage) PresignPutURL(context.Context, string, int64, string) (string, error) {
	return "", f.err
}
func (f *fakeStorage) PresignGetURL(_ context.Context, key string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "signed://" + key, nil
}
func (f *fakeStorage) PresignAvatarURL(context.Context, string) (string, error) { return "", f.err }
func (f *fakeStorage) PresignGetURLWithExpiry(context.Context, string, time.Duration) (string, error) {
	return "", f.err
}
func (f *fakeStorage) StatObject(context.Context, string) (int64, error) { return 0, f.err }
func (f *fakeStorage) DeleteObject(context.Context, string) error        { return f.err }
func (f *fakeStorage) DownloadToFile(context.Context, string, string) error {
	return f.err
}
func (f *fakeStorage) UploadFromFile(context.Context, string, string, int64) error { return f.err }

// TestBusinessErrorsDoNotTripBreaker 锁死一个实测踩到的坑：
// StatObject 返回"对象不存在"是健康依赖的正常答复，若计成失败，
// 一次正常的 404 就会打开熔断器（S3 恢复后的 half-open 探测因此失败过）。
func TestBusinessErrorsDoNotTripBreaker(t *testing.T) {
	clock := newFakeClock()
	cfg := testConfig()
	cfg.IsFailure = func(err error) bool { return !errors.Is(err, errBusiness) }
	b := New(cfg, WithClock(clock.Now))

	for range 10 {
		require.ErrorIs(t, b.Do(context.Background(), func(context.Context) error { return errBusiness }), errBusiness)
	}
	require.Equal(t, StateClosed, b.State(), "业务错误不应触发熔断")

	// 真正的依赖故障仍然会触发
	for range 3 {
		require.Error(t, b.Do(context.Background(), func(context.Context) error { return errors.New("timeout") }))
	}
	require.Equal(t, StateOpen, b.State())
}

var errBusiness = errors.New("object not found")

func (f *fakeStorage) FinalizeUpload(context.Context, string, string, int64) error { return f.err }
