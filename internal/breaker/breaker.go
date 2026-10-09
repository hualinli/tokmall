// Package breaker 实现依赖级熔断器。
//
// 与"限流"的区别（面试常问）：
//   - 限流保护的是**自己**：控制入口流量，避免被压垮（TokMall 已有三档限流）；
//   - 熔断保护的是**调用方**：当下游持续失败时快速失败，不再把线程/连接
//     耗在必然失败的等待上，给下游恢复的时间。
//
// 状态机（经典三态）：
//
//	closed   ──连续失败/失败率超阈值──▶ open
//	open     ──冷却期结束──────────▶ half-open
//	half-open──探测成功──▶ closed（恢复）
//	half-open──探测失败──▶ open（冷却期翻倍）
//
// 为什么要有 half-open：直接 closed 会让恢复瞬间的全部流量再次打垮下游；
// half-open 只放行少量探测请求，成功才恢复。
package breaker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrOpen 表示熔断器处于打开状态，调用被快速拒绝。
// 调用方应当把它当成"依赖暂时不可用"处理（降级或返回 503），
// 而不是当成业务错误。
var ErrOpen = errors.New("breaker is open")

type State string

const (
	StateClosed   State = "closed"
	StateOpen     State = "open"
	StateHalfOpen State = "half_open"
)

type Config struct {
	// Name 用于指标与日志。
	Name string
	// FailureThreshold 是"连续失败多少次后打开"。连续失败比失败率更简单，
	// 也更适合低频依赖（失败率在小样本上没有意义）。
	FailureThreshold int
	// MinRequests 是统计失败率前要求的最小样本数（0 表示只看连续失败）。
	MinRequests int
	// FailureRatio 是失败率阈值（配合 MinRequests 使用，0 表示不启用）。
	FailureRatio float64
	// Window 是失败率统计窗口。
	Window time.Duration
	// OpenDuration 是初始冷却期；连续打开会指数退避（上限 MaxOpenDuration）。
	OpenDuration time.Duration
	// MaxOpenDuration 是冷却期上限。
	MaxOpenDuration time.Duration
	// HalfOpenProbes 是 half-open 状态下允许并发放行的探测请求数。
	HalfOpenProbes int
	// IsFailure 判定"这个错误是否算依赖故障"。nil 表示所有错误都算。
	//
	// 为什么必须有它：StatObject 返回"对象不存在"是**健康依赖的正常答复**，
	// 不是故障。如果把它计成失败，一次正常的 404 就能把熔断器打开——
	// 实测踩过：S3 恢复后的 half-open 探测因为返回 NoSuchKey 被判失败，
	// 熔断器立刻重新打开。
	IsFailure func(error) bool
}

// isFailure 按配置判定错误性质。
func (c Config) isFailure(err error) bool {
	if err == nil {
		return false
	}
	if c.IsFailure == nil {
		return true
	}
	return c.IsFailure(err)
}

func (c Config) withDefaults() Config {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.Window <= 0 {
		c.Window = 30 * time.Second
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = 5 * time.Second
	}
	if c.MaxOpenDuration <= 0 {
		c.MaxOpenDuration = 60 * time.Second
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = 1
	}
	return c
}

// Stats 是熔断器的指标接口（实现在 observability）。
type Stats interface {
	State(name string, state State)
	Opened(name string)
	Rejected(name string)
	Probe(name string, success bool)
}

type NopStats struct{}

func (NopStats) State(string, State) {}
func (NopStats) Opened(string)       {}
func (NopStats) Rejected(string)     {}
func (NopStats) Probe(string, bool)  {}

// Breaker 是单个依赖的熔断器。并发安全：状态与窗口计数都在锁内维护。
type Breaker struct {
	cfg   Config
	stats Stats
	now   func() time.Time

	mu               sync.Mutex
	state            State
	consecutiveFails int
	openedAt         time.Time
	openCount        int
	// 滑动窗口内的样本（失败率统计用）
	windowStart time.Time
	windowTotal int
	windowFails int
	// half-open 期间已放行的探测数
	probesInFlight int
}

func New(cfg Config, opts ...Option) *Breaker {
	cfg = cfg.withDefaults()
	b := &Breaker{cfg: cfg, stats: NopStats{}, now: time.Now, state: StateClosed}
	for _, opt := range opts {
		opt(b)
	}
	b.stats.State(cfg.Name, b.state)
	return b
}

type Option func(*Breaker)

func WithStats(stats Stats) Option { return func(b *Breaker) { b.stats = stats } }

// WithClock 注入时钟，便于测试不依赖真实时间。
func WithClock(now func() time.Time) Option { return func(b *Breaker) { b.now = now } }

// State 返回当前状态（供健康检查/指标使用）。
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state
}

// Do 执行一次受保护调用。被熔断时**不会执行** fn，直接返回 ErrOpen。
func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	if err := b.before(); err != nil {
		return err
	}
	err := fn(ctx)
	b.after(err)
	return err
}

// Execute 是泛型版本，省去调用方写闭包变量的样板代码。
func Execute[T any](ctx context.Context, b *Breaker, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := b.before(); err != nil {
		return zero, err
	}
	value, err := fn(ctx)
	b.after(err)
	return value, err
}

// before 决定这次调用是否放行。
func (b *Breaker) before() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()

	switch b.state {
	case StateOpen:
		b.stats.Rejected(b.cfg.Name)
		return ErrOpen
	case StateHalfOpen:
		if b.probesInFlight >= b.cfg.HalfOpenProbes {
			// 探测名额已满：多余的请求同样快速失败，避免恢复瞬间打垮下游。
			b.stats.Rejected(b.cfg.Name)
			return ErrOpen
		}
		b.probesInFlight++
	}
	return nil
}

// after 根据调用结果推进状态机。
func (b *Breaker) after(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	failed := b.cfg.isFailure(err)

	if b.state == StateHalfOpen {
		b.probesInFlight--
		if !failed {
			// 探测成功（依赖给出了正常答复）→ 恢复
			b.state = StateClosed
			b.consecutiveFails = 0
			b.openCount = 0
			b.resetWindowLocked()
			b.stats.Probe(b.cfg.Name, true)
			b.stats.State(b.cfg.Name, b.state)
			return
		}
		b.stats.Probe(b.cfg.Name, false)
		b.tripLocked()
		return
	}

	b.recordSampleLocked(failed)
	if !failed {
		b.consecutiveFails = 0
		return
	}
	b.consecutiveFails++
	if b.shouldTripLocked() {
		b.tripLocked()
	}
}

func (b *Breaker) recordSampleLocked(failed bool) {
	now := b.now()
	if b.windowStart.IsZero() || now.Sub(b.windowStart) > b.cfg.Window {
		b.resetWindowLocked()
		b.windowStart = now
	}
	b.windowTotal++
	if failed {
		b.windowFails++
	}
}

func (b *Breaker) resetWindowLocked() {
	b.windowStart = b.now()
	b.windowTotal = 0
	b.windowFails = 0
}

func (b *Breaker) shouldTripLocked() bool {
	if b.consecutiveFails >= b.cfg.FailureThreshold {
		return true
	}
	if b.cfg.MinRequests > 0 && b.cfg.FailureRatio > 0 && b.windowTotal >= b.cfg.MinRequests {
		return float64(b.windowFails)/float64(b.windowTotal) >= b.cfg.FailureRatio
	}
	return false
}

// tripLocked 打开熔断器，冷却期按连续打开次数指数退避。
func (b *Breaker) tripLocked() {
	b.state = StateOpen
	b.openedAt = b.now()
	b.openCount++
	b.stats.Opened(b.cfg.Name)
	b.stats.State(b.cfg.Name, b.state)
}

// maybeHalfOpenLocked 在冷却期结束后进入 half-open。
func (b *Breaker) maybeHalfOpenLocked() {
	if b.state != StateOpen {
		return
	}
	if b.now().Sub(b.openedAt) < b.openDurationLocked() {
		return
	}
	b.state = StateHalfOpen
	b.probesInFlight = 0
	b.stats.State(b.cfg.Name, b.state)
}

func (b *Breaker) openDurationLocked() time.Duration {
	duration := b.cfg.OpenDuration
	for i := 1; i < b.openCount; i++ {
		duration *= 2
		if duration >= b.cfg.MaxOpenDuration {
			return b.cfg.MaxOpenDuration
		}
	}
	return duration
}

// Snapshot 暴露内部计数，供测试与排障使用。
type Snapshot struct {
	State            State
	ConsecutiveFails int
	WindowTotal      int
	WindowFails      int
	OpenCount        int
}

func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return Snapshot{
		State:            b.state,
		ConsecutiveFails: b.consecutiveFails,
		WindowTotal:      b.windowTotal,
		WindowFails:      b.windowFails,
		OpenCount:        b.openCount,
	}
}
