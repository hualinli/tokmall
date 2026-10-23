package outbox

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DirectConsumer 用 FOR UPDATE SKIP LOCKED 从 outbox 表取事件，在同一进程里写回执和投影。
// 它不做 Redis Streams 这一跳。和中继加 Streams 消费者二选一，否则同一条事件会被两边认领。
type DirectConsumer struct {
	repo        Repository
	db          *gorm.DB
	group       string
	handlers    map[string]Handler
	interval    time.Duration
	batch       int
	lease       time.Duration
	maxTries    int32
	baseBackoff time.Duration
	stats       ConsumerStats
}

func NewDirectConsumer(repo Repository, db *gorm.DB, group string, handlers []Handler, interval time.Duration, batch int, lease time.Duration, maxTries int32) *DirectConsumer {
	byType := map[string]Handler{}
	for _, handler := range handlers {
		for _, eventType := range handler.EventTypes() {
			byType[eventType] = handler
		}
	}
	if batch <= 0 {
		batch = 32
	}
	if interval <= 0 {
		interval = time.Second
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	if maxTries <= 0 {
		maxTries = 5
	}
	return &DirectConsumer{
		repo: repo, db: db, group: group, handlers: byType,
		interval: interval, batch: batch, lease: lease, maxTries: maxTries,
		baseBackoff: time.Second, stats: NopConsumerStats{},
	}
}

func (c *DirectConsumer) Run(ctx context.Context) {
	slog.InfoContext(ctx, "outbox direct consumer started", "group", c.group)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "outbox direct consumer stopped")
			return
		case <-ticker.C:
			for {
				processed, err := c.RunOnce(ctx)
				if err != nil {
					slog.ErrorContext(ctx, "outbox direct consumer batch failed", "err", err)
					break
				}
				if processed < c.batch || ctx.Err() != nil {
					break
				}
			}
		}
	}
}

// RunOnce 处理一批到期事件。返回值是本批认领到的条数，不满一批表示当前没有积压。
func (c *DirectConsumer) RunOnce(ctx context.Context) (int, error) {
	now := time.Now()
	events, err := c.repo.FetchPending(ctx, c.batch, c.lease, now)
	if err != nil {
		return 0, err
	}
	for _, event := range events {
		c.consume(ctx, event)
	}
	return len(events), nil
}

func (c *DirectConsumer) consume(ctx context.Context, event Event) {
	handler, ok := c.handlers[event.EventType]
	if !ok {
		if err := c.repo.MarkPublished(ctx, []int64{event.ID}, time.Now()); err != nil {
			slog.ErrorContext(ctx, "mark unhandled outbox event published failed", "event_id", event.EventID, "err", err)
		}
		return
	}
	duplicated := false
	txErr := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		receipt := consumedEvent{
			Consumer: c.group, EventID: event.EventID, EventType: event.EventType,
			AggregateID: event.AggregateID,
		}
		inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected == 0 {
			duplicated = true
			return nil
		}
		return handler.Handle(ctx, tx, event)
	})
	if txErr != nil {
		c.stats.Failed(event.EventType)
		c.retryOrDead(ctx, event, txErr)
		return
	}
	if err := c.repo.MarkPublished(ctx, []int64{event.ID}, time.Now()); err != nil {
		// 回执已提交。标记失败会在租约到期后重投，回执冲突使投影不会再写一次。
		slog.ErrorContext(ctx, "mark directly consumed event published failed", "event_id", event.EventID, "err", err)
		return
	}
	if duplicated {
		c.stats.Duplicated(event.EventType)
		return
	}
	c.stats.Processed(event.EventType)
}

func (c *DirectConsumer) retryOrDead(ctx context.Context, event Event, cause error) {
	if event.Attempts >= c.maxTries {
		if err := c.repo.MarkDead(ctx, event.ID, event.Attempts, cause.Error()); err != nil {
			slog.ErrorContext(ctx, "mark outbox event dead failed", "event_id", event.EventID, "err", err)
			return
		}
		c.stats.DeadLettered(event.EventType)
		slog.ErrorContext(ctx, "outbox event moved to dead", "event_id", event.EventID, "event_type", event.EventType, "err", cause)
		return
	}
	shift := c.baseBackoff
	for i := int32(1); i < event.Attempts; i++ {
		shift *= 2
	}
	if err := c.repo.MarkRetry(ctx, event.ID, event.Attempts, time.Now().Add(shift), cause.Error()); err != nil {
		slog.ErrorContext(ctx, "mark outbox retry failed", "event_id", event.EventID, "err", err)
	}
}
