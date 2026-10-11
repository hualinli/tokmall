package outbox

import (
	"context"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository 是中继与运维命令使用的事件存储。
type Repository interface {
	// FetchPending 取一批到期事件并加租约（FOR UPDATE SKIP LOCKED，支持多实例）。
	FetchPending(ctx context.Context, batch int, lease time.Duration, now time.Time) ([]Event, error)
	MarkPublished(ctx context.Context, ids []int64, at time.Time) error
	DeferPending(ctx context.Context, events []Event, until time.Time) error
	MarkRetry(ctx context.Context, id int64, attempts int32, nextAttempt time.Time, reason string) error
	MarkDead(ctx context.Context, id int64, attempts int32, reason string) error
	Stats(ctx context.Context) (*Stats, error)
	ListDead(ctx context.Context, limit int) ([]Event, error)
	// Requeue 把 DEAD 事件重新放回 PENDING（人工重放）。
	Requeue(ctx context.Context, eventID string, now time.Time) (bool, error)
}

type Stats struct {
	// PendingDue 是"已到期、等着被投递"的事件数——它才是真正的积压。
	PendingDue int64
	// PendingDelayed 是"还没到期"的事件数（例如订单超时事件，available_at
	// 设在 expires_at）。把它和积压分开统计，否则每次下单都会让监控报"积压"，
	// 真积压反而被淹没。
	PendingDelayed int64
	Dead           int64
	// OldestPendingAge 是最早一条**已到期**事件的年龄，对应"积压了多久"。
	OldestPendingAge time.Duration
}

var (
	ErrEventNotFound = errors.New("outbox event not found")
	ErrEventNotDead  = errors.New("outbox event is not in DEAD state")
)

type GormRepository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *GormRepository { return &GormRepository{db: db} }

// FetchPending 一次拿到租约并返回事件：
//
//	SELECT ... WHERE status='PENDING' AND available_at <= now()
//	ORDER BY id LIMIT n FOR UPDATE SKIP LOCKED
//
// 租约（leased_until）而不是"取走即删除"：中继崩溃时事件仍在表里，
// 租约过期后会被下一个实例重新取走，因此投递语义是至少一次。
func (r *GormRepository) FetchPending(ctx context.Context, batch int, lease time.Duration, now time.Time) ([]Event, error) {
	var events []Event
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND available_at <= ?", string(StatusPending), now.UTC()).
			Where("leased_until IS NULL OR leased_until <= ?", now.UTC()).
			Order("id ASC").Limit(batch).Find(&events).Error
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		ids := make([]int64, len(events))
		for i := range events {
			ids[i] = events[i].ID
		}
		leased := now.UTC().Add(lease)
		return tx.Model(&Event{}).Where("id IN ?", ids).
			Updates(map[string]any{"leased_until": leased, "attempts": gorm.Expr("attempts + 1")}).Error
	})
	if err != nil {
		return nil, err
	}
	// 返回的 attempts 反映"本次投递是第几次尝试"，与数据库自增保持一致。
	for i := range events {
		events[i].Attempts++
	}
	return events, nil
}

// DeferPending 释放未投递租约；背压不消耗失败重试次数。
// attempts 防止旧持有者释放后来被另一中继认领的事件。
func (r *GormRepository) DeferPending(ctx context.Context, events []Event, until time.Time) error {
	if len(events) == 0 {
		return nil
	}
	pairs := make([][]any, 0, len(events))
	for _, event := range events {
		pairs = append(pairs, []any{event.ID, event.Attempts})
	}
	return r.db.WithContext(ctx).Model(&Event{}).
		Where("status = ? AND (id, attempts) IN ?", string(StatusPending), pairs).
		Updates(map[string]any{"leased_until": nil, "available_at": until.UTC(),
			"attempts": gorm.Expr("GREATEST(attempts - 1, 0)")}).Error
}

func (r *GormRepository) MarkPublished(ctx context.Context, ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&Event{}).Where("id IN ?", ids).
		Updates(map[string]any{
			"status": string(StatusPublished), "published_at": at.UTC(),
			"leased_until": nil, "last_error": nil,
		}).Error
}

func (r *GormRepository) MarkRetry(ctx context.Context, id int64, attempts int32, nextAttempt time.Time, reason string) error {
	return r.db.WithContext(ctx).Model(&Event{}).Where("id = ?", id).
		Updates(map[string]any{
			"available_at": nextAttempt.UTC(), "leased_until": nil,
			"attempts": attempts, "last_error": truncateError(reason),
		}).Error
}

func (r *GormRepository) MarkDead(ctx context.Context, id int64, attempts int32, reason string) error {
	return r.db.WithContext(ctx).Model(&Event{}).Where("id = ?", id).
		Updates(map[string]any{
			"status": string(StatusDead), "leased_until": nil,
			"attempts": attempts, "last_error": truncateError(reason),
		}).Error
}

func (r *GormRepository) Stats(ctx context.Context) (*Stats, error) {
	stats := &Stats{}
	row := r.db.WithContext(ctx).Raw(`
		SELECT
			count(*) FILTER (WHERE status = 'PENDING' AND available_at <= now()) AS pending_due,
			count(*) FILTER (WHERE status = 'PENDING' AND available_at > now()) AS pending_delayed,
			count(*) FILTER (WHERE status = 'DEAD') AS dead,
			COALESCE(EXTRACT(EPOCH FROM now() - min(available_at) FILTER (WHERE status = 'PENDING' AND available_at <= now())), 0) AS oldest_age
		FROM outbox_events`).Row()
	var oldest float64
	if err := row.Scan(&stats.PendingDue, &stats.PendingDelayed, &stats.Dead, &oldest); err != nil {
		return nil, err
	}
	if oldest > 0 {
		stats.OldestPendingAge = time.Duration(oldest * float64(time.Second))
	}
	return stats, nil
}

func (r *GormRepository) ListDead(ctx context.Context, limit int) ([]Event, error) {
	var events []Event
	err := r.db.WithContext(ctx).Where("status = ?", string(StatusDead)).
		Order("id DESC").Limit(limit).Find(&events).Error
	if err != nil {
		return nil, err
	}
	return events, nil
}

// Requeue 把 DEAD 事件放回 PENDING 并清零退避。event_id 不变，
// 因此消费侧仍按原幂等键去重——重放不会造成二次投影。
func (r *GormRepository) Requeue(ctx context.Context, eventID string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Model(&Event{}).
		Where("event_id = ? AND status = ?", eventID, string(StatusDead)).
		Updates(map[string]any{
			"status": string(StatusPending), "available_at": now.UTC(),
			"leased_until": nil, "attempts": 0, "last_error": nil,
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		// 区分"不存在"和"存在但不是 DEAD"，让运维知道为什么没重放成功。
		var count int64
		if err := r.db.WithContext(ctx).Model(&Event{}).Where("event_id = ?", eventID).Count(&count).Error; err != nil {
			return false, err
		}
		if count == 0 {
			return false, ErrEventNotFound
		}
		return false, ErrEventNotDead
	}
	return true, nil
}

// Enqueue 在调用方事务内写入事件。它是本包唯一被业务模块调用的入口：
// 传入的 *gorm.DB 必须是业务事务的句柄，否则就退化成"提交后发消息"。
func Enqueue(tx *gorm.DB, event Event) error {
	if tx == nil {
		return errors.New("outbox enqueue requires a transaction handle")
	}
	return tx.Create(&event).Error
}

// EnqueueOrderPaid / EnqueueOrderRefunded 是给订单模块用的构造入口，
// 把事件类型、聚合类型和分区键的约定收在一处。
func EnqueueOrderPaid(tx *gorm.DB, payload OrderPaidPayload) error {
	partition := "order"
	if payload.SourceAuthorID != nil {
		partition = "author:" + strconv.FormatInt(*payload.SourceAuthorID, 10)
	}
	event, err := newEvent(EventTypeOrderPaid, AggregateTypeOrder, payload.OrderID, partition, payload)
	if err != nil {
		return err
	}
	return Enqueue(tx, event)
}

// EnqueueOrderTimeout 在订单创建事务里写入超时事件（延迟到 expires_at 才投递）。
//
// 与"提交订单后再注册定时任务"的区别：事件与订单在同一事务里，进程崩溃
// 不会导致"订单存在但没有超时事件"；反过来订单回滚时事件也一起消失。
func EnqueueOrderTimeout(tx *gorm.DB, payload OrderTimeoutPayload) error {
	event, err := newDelayedEvent(EventTypeOrderTimeout, AggregateTypeOrder, payload.OrderID,
		"order:"+strconv.FormatInt(payload.OrderID, 10), payload.ExpiresAt, payload)
	if err != nil {
		return err
	}
	return Enqueue(tx, event)
}

func EnqueueOrderRefunded(tx *gorm.DB, payload OrderRefundedPayload) error {
	partition := "order"
	if payload.SourceAuthorID != nil {
		partition = "author:" + strconv.FormatInt(*payload.SourceAuthorID, 10)
	}
	event, err := newEvent(EventTypeOrderRefunded, AggregateTypeOrder, payload.OrderID, partition, payload)
	if err != nil {
		return err
	}
	return Enqueue(tx, event)
}

// maxErrorLength 与 last_error 的用途匹配：留够排查信息，又不让一条
// 反复失败的巨大错误文本把行撑大。
const maxErrorLength = 500

// truncateError 按字节上限（行大小）截断，但只在 rune 边界落刀：切在多字节字符中间会得到
// 非法 UTF-8，PostgreSQL 拒绝整条 UPDATE，失败记录与 attempts 都写不进去。
func truncateError(reason string) string {
	if len(reason) <= maxErrorLength {
		return reason
	}
	cut := maxErrorLength
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}
