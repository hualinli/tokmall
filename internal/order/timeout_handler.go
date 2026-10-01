package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lihualin/tokmall/internal/outbox"
	"gorm.io/gorm"
)

// TimeoutHandler 消费"订单超时"延迟事件：把到点仍未支付的订单关掉。
//
// 这是"延迟消息加速器 + 轮询 Worker 安全网"里的加速器那一半：
//
//	下单事务 ──写 outbox（available_at = expires_at）──> 中继到点投递 ──> 本 handler
//	    └── 同时 expires_at 已落库，轮询 Worker 每 5s 扫一次作为安全网
//
// 为什么不能只靠延迟消息（这是 DomTok 的教训）：消息可能丢（broker 无持久化、
// 保留期过期、消费端停机超过保留窗口）。一旦"事实源"是消息，丢一条就意味着一笔
// 订单的库存永久泄漏。所以这里把**数据库当唯一事实源**，消息只负责"让关单更早
// 发生"；消息全丢，系统仍在轮询间隔内收敛。
//
// 幂等性来自条件更新：关单只在 status='PENDING_PAYMENT' AND expires_at <= now()
// 时生效，已支付/已取消/已过期的订单会拿到 0 行受影响，handler 视为成功（no-op）。
type TimeoutHandler struct {
	db    *gorm.DB
	stats TimeoutStats
	now   func() time.Time
}

// TimeoutStats 是超时事件处理的指标接口（实现在 observability）。
type TimeoutStats interface {
	TimeoutEvent(result string)
}

type NopTimeoutStats struct{}

func (NopTimeoutStats) TimeoutEvent(string) {}

func NewTimeoutHandler(db *gorm.DB, opts ...TimeoutOption) *TimeoutHandler {
	handler := &TimeoutHandler{db: db, stats: NopTimeoutStats{}, now: time.Now}
	for _, opt := range opts {
		opt(handler)
	}
	return handler
}

type TimeoutOption func(*TimeoutHandler)

func WithTimeoutStats(stats TimeoutStats) TimeoutOption {
	return func(h *TimeoutHandler) { h.stats = stats }
}

func (h *TimeoutHandler) EventTypes() []string {
	return []string{outbox.EventTypeOrderTimeout}
}

// Handle 在消费者的事务里执行关单。
//
// 注意它拿到的 tx 由 outbox 消费者提供：消费回执与关单在同一事务里提交，
// 因此"重复投递"不会重复回补库存（回执冲突时根本不会调用到这里），
// 而"关单失败"会让整条消息留在 PEL 里重试。
func (h *TimeoutHandler) Handle(ctx context.Context, tx *gorm.DB, event outbox.Event) error {
	var payload outbox.OrderTimeoutPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		// 载荷无法解析说明事件格式坏了，重试也不会成功：
		// 返回错误会让消息反复重投直到进 DLQ——这正是我们想要的可见性。
		return fmt.Errorf("decode order timeout payload: %w", err)
	}
	if payload.OrderID <= 0 {
		return errors.New("order timeout payload has invalid order id")
	}

	expired, err := h.expireIfDue(ctx, tx, payload.OrderID)
	if err != nil {
		h.stats.TimeoutEvent("failed")
		slog.Error("order timeout handling failed", "order_id", payload.OrderID, "err", err)
		return err
	}
	if !expired {
		// 订单已被支付/取消/由 Worker 关掉：这是最常见的"正常竞争结果"，
		// 不是错误。记 noop 而不是 failed，否则监控会被假失败淹没。
		h.stats.TimeoutEvent("noop")
		return nil
	}
	h.stats.TimeoutEvent("expired")
	slog.Info("order expired by delayed event", "order_id", payload.OrderID)
	return nil
}

// expireIfDue 用一条条件更新 + 一次库存回补完成关单，全部在调用方事务内。
// 返回 false 表示"订单已经不在待支付状态"，即无事可做。
func (h *TimeoutHandler) expireIfDue(ctx context.Context, tx *gorm.DB, orderID int64) (bool, error) {
	var order Order
	err := tx.WithContext(ctx).Clauses().Where("id = ?", orderID).Take(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 订单不存在（例如被硬删）：视为无事可做，但留一条日志便于排查。
		slog.Warn("order timeout event for missing order", "order_id", orderID)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if order.Status != StatusPendingPayment {
		return false, nil
	}
	if h.now().UTC().Before(order.ExpiresAt) {
		// 时钟回拨或事件被提前投递：不动它，等真正到点。
		return false, nil
	}
	if err := expireLockedOrder(tx.WithContext(ctx), order, h.now().UTC()); err != nil {
		if errors.Is(err, ErrOrderStateConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

var _ outbox.Handler = (*TimeoutHandler)(nil)
