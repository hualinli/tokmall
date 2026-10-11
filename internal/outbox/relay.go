package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Relay 是 Outbox 中继：把 PENDING 事件投递给传输层，并维护重试与 DLQ。
//
// 投递语义是"至少一次"：先投递、后标记，两者之间崩溃会重发同一 event_id。
// 这是刻意选择——精确一次需要分布式事务或 broker 事务，代价远高于让消费方
// 做一次幂等去重（consumed_events 主键）。
type Relay struct {
	repo      Repository
	publisher Publisher
	interval  time.Duration
	batch     int
	lease     time.Duration
	maxTries  int32
	// baseBackoff 是指数退避的起点：第 n 次失败后等待 base * 2^(n-1)。
	baseBackoff time.Duration
	// stats 是可选指标回调（由 observability 注入，避免 outbox 依赖 prometheus）。
	stats RelayStats
}

// RelayStats 是消费侧定义的指标接口（与项目其它模块的注入风格一致）。
type RelayStats interface {
	Published(eventType string, count int)
	Failed(eventType string)
	Dead(eventType string)
	Pending(due, delayed, dead int64, oldestAge time.Duration)
}

type NopRelayStats struct{}

func (NopRelayStats) Published(string, int)                      {}
func (NopRelayStats) Failed(string)                              {}
func (NopRelayStats) Dead(string)                                {}
func (NopRelayStats) Pending(int64, int64, int64, time.Duration) {}

type RelayOption func(*Relay)

func WithRelayStats(stats RelayStats) RelayOption {
	return func(r *Relay) { r.stats = stats }
}

// WithRelayBackoff 调整重试退避起点（测试里设 0 可以立即重试，
// 不必等待真实的 1s/2s/4s 退避）。
func WithRelayBackoff(base time.Duration) RelayOption {
	return func(r *Relay) { r.baseBackoff = base }
}

func NewRelay(repo Repository, publisher Publisher, interval time.Duration, batch int, lease time.Duration, maxTries int32, opts ...RelayOption) *Relay {
	relay := &Relay{
		repo: repo, publisher: publisher, interval: interval, batch: batch,
		lease: lease, maxTries: maxTries, baseBackoff: time.Second, stats: NopRelayStats{},
	}
	for _, opt := range opts {
		opt(relay)
	}
	return relay
}

// Run 周期投递直到 ctx 取消。每个 tick 处理一批，处理完立即进入下一轮
// （积压时不会白等一个 interval）。
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("outbox relay stopped")
			return
		case <-ticker.C:
			for {
				processed, err := r.RunOnce(ctx)
				if err != nil {
					slog.Error("outbox relay batch failed", "err", err)
					break
				}
				if processed < r.batch {
					break
				}
				// 还有积压：继续处理，直到取不满一批或上下文取消。
				if ctx.Err() != nil {
					return
				}
			}
			r.reportStats(ctx)
		}
	}
}

// RunOnce 处理一批事件，返回处理条数（供测试与运维手动触发）。
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	now := time.Now()
	events, err := r.repo.FetchPending(ctx, r.batch, r.lease, now)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	published := make([]int64, 0, len(events))
	backpressured := false
	for index, event := range events {
		msg := Message{
			EventID: event.EventID.String(), EventType: event.EventType,
			AggregateType: event.AggregateType, AggregateID: event.AggregateID,
			PartitionKey: event.PartitionKey, Payload: event.Payload, OccurredAt: event.CreatedAt,
		}
		if err := r.publisher.Publish(ctx, msg); err != nil {
			if errors.Is(err, ErrStreamBackpressure) {
				backpressured = true
				if deferErr := r.repo.DeferPending(ctx, events[index:], time.Now().Add(r.interval)); deferErr != nil {
					return len(events), deferErr
				}
				slog.WarnContext(ctx, "outbox stream backpressure", "deferred", len(events)-index)
				break
			}
			r.handleFailure(ctx, event, err)
			continue
		}
		published = append(published, event.ID)
		r.stats.Published(event.EventType, 1)
	}
	if err := r.repo.MarkPublished(ctx, published, time.Now()); err != nil {
		// 投递成功但标记失败：事件会在租约到期后被重投。
		// 这正是"至少一次"的来源，消费侧必须幂等。
		slog.Error("mark outbox published failed; events will be redelivered",
			"count", len(published), "err", err)
		return len(events), err
	}
	// 背压返回未满批，让Run等下个tick，避免空转。
	if backpressured {
		return len(published), nil
	}
	return len(events), nil
}

func (r *Relay) handleFailure(ctx context.Context, event Event, publishErr error) {
	r.stats.Failed(event.EventType)
	if event.Attempts >= r.maxTries {
		// 超过上限进 DLQ：不删除、不静默丢弃，等人工判断是重放还是作废。
		if err := r.repo.MarkDead(ctx, event.ID, event.Attempts, publishErr.Error()); err != nil {
			slog.Error("mark outbox dead failed", "event_id", event.EventID, "err", err)
			return
		}
		r.stats.Dead(event.EventType)
		slog.Error("outbox event moved to DLQ",
			"event_id", event.EventID, "event_type", event.EventType,
			"attempts", event.Attempts, "err", publishErr)
		return
	}
	backoff := r.backoff(event.Attempts)
	next := time.Now().Add(backoff)
	if err := r.repo.MarkRetry(ctx, event.ID, event.Attempts, next, publishErr.Error()); err != nil {
		slog.Error("mark outbox retry failed", "event_id", event.EventID, "err", err)
		return
	}
	slog.Warn("outbox publish failed, scheduled retry",
		"event_id", event.EventID, "attempts", event.Attempts, "backoff", backoff, "err", publishErr)
}

// backoff 指数退避：1s、2s、4s、8s……上限 1 分钟。
// 上限的存在是为了让"broker 恢复"能在可接受时间内被自动发现。
func (r *Relay) backoff(attempts int32) time.Duration {
	if attempts <= 0 {
		return r.baseBackoff
	}
	backoff := r.baseBackoff
	for i := int32(1); i < attempts; i++ {
		backoff *= 2
		if backoff >= time.Minute {
			return time.Minute
		}
	}
	return backoff
}

func (r *Relay) reportStats(ctx context.Context) {
	stats, err := r.repo.Stats(ctx)
	if err != nil {
		slog.Warn("outbox stats failed", "err", err)
		return
	}
	r.stats.Pending(stats.PendingDue, stats.PendingDelayed, stats.Dead, stats.OldestPendingAge)
	// 只对"已到期仍未投递"告警：未到期的是延迟事件（如订单超时），属正常状态。
	if stats.PendingDue > 0 {
		slog.Warn("outbox backlog", "pending_due", stats.PendingDue,
			"pending_delayed", stats.PendingDelayed, "dead", stats.Dead,
			"oldest_pending_age", stats.OldestPendingAge)
	}
}
