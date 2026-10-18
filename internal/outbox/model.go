// Package outbox 实现 Transactional Outbox 与至少一次事件消费。
//
// 它解决的问题是"业务事实已经提交、但下游副作用还没发生"这一段窗口：
// 支付事务把事件写进 outbox_events（同一事务，原子），中继进程再异步投递；
// 投递失败会重试并最终进入 DEAD（DLQ），消费方用 consumed_events 去重。
//
// 为什么不用"提交订单后再发消息"：进程在提交与发送之间崩溃会永久丢事件；
// 为什么不把副作用放进同一个事务：通知、统计、风控是跨系统的，
// 不可能共享本地事务，硬凑就是分布式事务。
package outbox

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// 事件类型常量。版本号进类型名：消费方按类型订阅，语义变更时新增版本，
// 而不是让老消费者解析新字段。
const (
	EventTypeOrderPaid     = "commerce.order.paid.v1"
	EventTypeOrderRefunded = "commerce.order.refunded.v1"
	// EventTypeOrderTimeout 是"延迟事件"：它在订单创建时写入，
	// 但 available_at 被设成 expires_at，因此中继要等到超时那一刻才会投递。
	// 它只是**加速器**——订单是否超时最终以数据库状态为准（条件更新），
	// 轮询 Worker 仍然是安全网。见 docs/architecture.md#order-expiry。
	EventTypeOrderTimeout = "commerce.order.timeout.v1"

	AggregateTypeOrder = "order"
)

type Status string

const (
	StatusPending   Status = "PENDING"
	StatusPublished Status = "PUBLISHED"
	// StatusDead 是超过最大重试次数的事件，等人工处理（DLQ）。
	StatusDead Status = "DEAD"
)

// Event 对应 outbox_events 表。
type Event struct {
	ID            int64           `gorm:"column:id;primaryKey"`
	EventID       uuid.UUID       `gorm:"column:event_id"`
	EventType     string          `gorm:"column:event_type"`
	AggregateType string          `gorm:"column:aggregate_type"`
	AggregateID   int64           `gorm:"column:aggregate_id"`
	PartitionKey  string          `gorm:"column:partition_key"`
	Payload       json.RawMessage `gorm:"column:payload"`
	Status        Status          `gorm:"column:status"`
	Attempts      int32           `gorm:"column:attempts"`
	AvailableAt   time.Time       `gorm:"column:available_at"`
	LeasedUntil   *time.Time      `gorm:"column:leased_until"`
	LastError     *string         `gorm:"column:last_error"`
	PublishedAt   *time.Time      `gorm:"column:published_at"`
	CreatedAt     time.Time       `gorm:"column:created_at"`
}

func (Event) TableName() string { return "outbox_events" }

// OrderPaidPayload 是 OrderPaid 事件的载荷。字段刻意冗余（作者、商品、
// 数量都在里面）：消费者不应该为了渲染一条事件再去回查订单表，
// 否则事件就失去了"自描述"的价值，也会把读模型和写入方耦合起来。
type OrderPaidPayload struct {
	OrderID         int64     `json:"order_id"`
	OrderNo         string    `json:"order_no"`
	UserID          int64     `json:"user_id"`
	MerchantID      int64     `json:"merchant_id"`
	TotalAmount     int64     `json:"total_amount"`
	Units           int32     `json:"units"`
	PaidAt          time.Time `json:"paid_at"`
	SourceAuthorID  *int64    `json:"source_author_id,omitempty"`
	SourceVideoID   *int64    `json:"source_video_id,omitempty"`
	SourceProductID *int64    `json:"source_product_id,omitempty"`
}

// OrderTimeoutPayload 是订单超时事件。载荷只带定位信息：
// 消费侧必须回查订单事实（状态、过期时间、订单项），不能凭事件内容关单。
type OrderTimeoutPayload struct {
	OrderID   int64     `json:"order_id"`
	OrderNo   string    `json:"order_no"`
	ExpiresAt time.Time `json:"expires_at"`
}

// OrderRefundedPayload 描述退款成功。它同时承载"退了多少钱"和"是否回补了
// 库存"两个事实，投影与对账都需要，不能靠消费者自己推断。
type OrderRefundedPayload struct {
	OrderID        int64     `json:"order_id"`
	OrderNo        string    `json:"order_no"`
	RefundNo       string    `json:"refund_no"`
	UserID         int64     `json:"user_id"`
	MerchantID     int64     `json:"merchant_id"`
	Amount         int64     `json:"amount"`
	Units          int32     `json:"units"`
	RestoreStock   bool      `json:"restore_stock"`
	RefundedAt     time.Time `json:"refunded_at"`
	SourceAuthorID *int64    `json:"source_author_id,omitempty"`
	SourceVideoID  *int64    `json:"source_video_id,omitempty"`
}

// newDelayedEvent 组装一条"到点才可见"的事件：available_at 决定中继最早
// 何时能取到它。这就是用 Outbox 实现延迟队列的方式——延迟能力来自数据库
// 字段与中继的扫描条件，不需要额外的调度中间件。
func newDelayedEvent(eventType, aggregateType string, aggregateID int64, partitionKey string, availableAt time.Time, payload any) (Event, error) {
	event, err := newEvent(eventType, aggregateType, aggregateID, partitionKey, payload)
	if err != nil {
		return Event{}, err
	}
	event.AvailableAt = availableAt.UTC()
	return event, nil
}

// newEvent 组装一条待投递事件。event_id 在写入时生成，之后重试、重放
// 都沿用同一个值——这是消费侧去重的唯一依据。
func newEvent(eventType, aggregateType string, aggregateID int64, partitionKey string, payload any) (Event, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return Event{}, err
	}
	return Event{
		EventID:       eventID,
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		PartitionKey:  partitionKey,
		Payload:       body,
		Status:        StatusPending,
	}, nil
}
