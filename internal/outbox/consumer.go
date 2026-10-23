package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Handler 是消费侧的业务投影：它在**调用方传入的事务**里写自己的读模型。
// 事务边界由 Consumer 掌握，因此"业务投影 + 消费回执"必然一起提交——
// 这是幂等消费能成立的前提。
type Handler interface {
	// EventTypes 声明该 handler 关心的事件类型；未声明的事件会被忽略并确认。
	EventTypes() []string
	Handle(ctx context.Context, tx *gorm.DB, event Event) error
}

// ConsumerStats 是消费侧指标接口。
type ConsumerStats interface {
	Processed(eventType string)
	Duplicated(eventType string)
	Failed(eventType string)
	DeadLettered(eventType string)
	Lag(pending int64)
}

type NopConsumerStats struct{}

func (NopConsumerStats) Processed(string)    {}
func (NopConsumerStats) Duplicated(string)   {}
func (NopConsumerStats) Failed(string)       {}
func (NopConsumerStats) DeadLettered(string) {}
func (NopConsumerStats) Lag(int64)           {}

// Consumer 是 Redis Streams 消费组消费者。
//
// 至少一次投递的完整闭环：
//  1. XREADGROUP 读取新消息（> 表示"我还没读过的"）；
//  2. 在数据库事务里写消费回执 + 业务投影，提交后 XACK；
//  3. 处理失败不 ACK：消息留在 PEL（待处理列表）；
//  4. 分页 XPENDING + XCLAIM 认领超时未确认的消息（消费者崩溃后的兜底）；
//  5. 投递次数超过上限 → 投到 DLQ Stream 并 ACK，等待人工重放。
type Consumer struct {
	db            *gorm.DB
	client        *redis.Client
	stream        string
	group         string
	dlqStream     string
	name          string
	handlers      map[string]Handler
	batch         int
	block         time.Duration
	claimIdle     time.Duration
	maxDeliveries int64
	stats         ConsumerStats
	claimMu       sync.Mutex
	claimStart    string
}

type ConsumerOption func(*Consumer)

func WithConsumerStats(stats ConsumerStats) ConsumerOption {
	return func(c *Consumer) { c.stats = stats }
}

// WithConsumerClaimIdle 设置"多久没 ACK 就认为消费者已失效"的阈值。
// 生产值是秒级（消费者崩溃后事件不该卡住太久），测试里可以设 0 立即认领。
func WithConsumerClaimIdle(idle time.Duration) ConsumerOption {
	return func(c *Consumer) { c.claimIdle = idle }
}

// WithConsumerBatch / WithConsumerBlock 调整单批条数与阻塞时长。
func WithConsumerBatch(batch int) ConsumerOption {
	return func(c *Consumer) { c.batch = batch }
}

func WithConsumerBlock(block time.Duration) ConsumerOption {
	return func(c *Consumer) { c.block = block }
}

func NewConsumer(db *gorm.DB, client *redis.Client, stream, group, dlqStream, name string, handlers []Handler, opts ...ConsumerOption) *Consumer {
	byType := map[string]Handler{}
	for _, handler := range handlers {
		for _, eventType := range handler.EventTypes() {
			byType[eventType] = handler
		}
	}
	consumer := &Consumer{
		db: db, client: client, stream: stream, group: group, dlqStream: dlqStream, name: name,
		handlers: byType, batch: 32, block: 2 * time.Second,
		claimIdle: 30 * time.Second, maxDeliveries: 5, stats: NopConsumerStats{},
	}
	for _, opt := range opts {
		opt(consumer)
	}
	return consumer
}

// Run 启动消费循环：先建组（幂等），再交替处理新消息与超时未确认消息。
func (c *Consumer) Run(ctx context.Context) {
	if err := c.ensureGroup(ctx); err != nil {
		slog.Error("outbox consumer cannot start", "group", c.group, "err", err)
		return
	}
	slog.Info("outbox consumer started", "stream", c.stream, "group", c.group, "consumer", c.name)
	claimTicker := time.NewTicker(c.claimIdle)
	defer claimTicker.Stop()
	for {
		if ctx.Err() != nil {
			slog.Info("outbox consumer stopped", "consumer", c.name)
			return
		}
		if _, err := c.ConsumeOnce(ctx); err != nil {
			slog.Error("consume batch failed", "err", err)
			time.Sleep(time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-claimTicker.C:
			if _, err := c.ClaimOnce(ctx); err != nil {
				slog.Warn("claim stale messages failed", "err", err)
			}
			c.reportLag(ctx)
		default:
		}
	}
}

// EnsureGroup 创建消费组。测试和直接调用 ConsumeOnce 的一方需要先建组；Run 自己会调。
func (c *Consumer) EnsureGroup(ctx context.Context) error { return c.ensureGroup(ctx) }

func (c *Consumer) ensureGroup(ctx context.Context) error {
	// MKSTREAM：组可以先于第一条消息存在，否则"先启动消费者再产生事件"会报错。
	err := c.client.XGroupCreateMkStream(ctx, c.stream, c.group, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return fmt.Errorf("create consumer group: %w", err)
	}
	return nil
}

// isBusyGroup：消费组已存在。启动时重复建组是正常情况，不是错误。
func isBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

// ConsumeOnce 读取并处理一批新消息，返回处理条数。
// Run 循环调用它；测试与 outboxctl 也可以单次调用（便于确定性验证）。
func (c *Consumer) ConsumeOnce(ctx context.Context) (int, error) {
	streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: c.group, Consumer: c.name,
		Streams: []string{c.stream, ">"},
		Count:   int64(c.batch), Block: c.block,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil // 阻塞超时，无新消息
		}
		return 0, fmt.Errorf("xreadgroup: %w", err)
	}
	processed := 0
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			c.process(ctx, msg)
			processed++
		}
	}
	return processed, nil
}

// ClaimOnce 有界扫描 PEL，并保留下一页位置，即使这一页没有超时消息。
// XPENDING 先读取同一页的次数；XCLAIM 再原子检查 MinIdle，避免抢走刚被
// 其它消费者续领的消息。查询失败不认领、不推进游标，也不把未知次数当作零。
func (c *Consumer) ClaimOnce(ctx context.Context) (int, error) {
	c.claimMu.Lock()
	defer c.claimMu.Unlock()
	start := c.claimStart
	if start == "" {
		start = "-"
	}
	pending, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: c.stream, Group: c.group, Start: start, End: "+", Count: int64(c.batch),
	}).Result()
	if err != nil {
		return 0, fmt.Errorf("read pending deliveries: %w", err)
	}
	next := "-"
	if len(pending) == c.batch {
		next = "(" + pending[len(pending)-1].ID
	}
	ids := make([]string, 0, len(pending))
	deliveries := make(map[string]int64, len(pending))
	for _, entry := range pending {
		if entry.Idle < c.claimIdle {
			continue
		}
		ids = append(ids, entry.ID)
		deliveries[entry.ID] = entry.RetryCount
	}
	if len(ids) == 0 {
		c.claimStart = next
		return 0, nil
	}
	messages, err := c.client.XClaim(ctx, &redis.XClaimArgs{
		Stream: c.stream, Group: c.group, Consumer: c.name, MinIdle: c.claimIdle, Messages: ids,
	}).Result()
	if err != nil {
		return 0, fmt.Errorf("xclaim: %w", err)
	}
	c.claimStart = next
	for _, msg := range messages {
		count, known := deliveries[msg.ID]
		if !known {
			return 0, fmt.Errorf("missing pending delivery count for %s", msg.ID)
		}
		if count > c.maxDeliveries {
			c.deadLetter(ctx, msg, count)
			continue
		}
		slog.WarnContext(ctx, "reclaiming stale message", "id", msg.ID, "deliveries", count)
		c.process(ctx, msg)
	}
	return len(messages), nil
}

// process 是幂等消费的核心：
//
//	BEGIN
//	  INSERT INTO consumed_events (consumer, event_id, ...) ON CONFLICT DO NOTHING
//	  -- 冲突 => 已经处理过，跳过业务写入（重复投递）
//	  handler.Handle(tx, event)
//	COMMIT
//	XACK
//
// 回执与投影同事务，因此不存在"投影写成功但回执没写"导致的重复应用。
func (c *Consumer) process(ctx context.Context, msg redis.XMessage) {
	event, err := decodeMessage(msg)
	if err != nil {
		// 无法解析的消息不可能靠重试修复：直接进 DLQ，避免卡住消费组。
		slog.Error("undecodable outbox message moved to DLQ", "id", msg.ID, "err", err)
		c.deadLetterRaw(ctx, msg, err.Error())
		return
	}
	handler, ok := c.handlers[event.EventType]
	if !ok {
		// 未订阅的类型：确认掉但不处理。保留在 PEL 只会无限重投。
		slog.Warn("no handler for event type, acking", "event_type", event.EventType, "event_id", event.EventID)
		c.ack(ctx, msg.ID)
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
		slog.Error("consume event failed, will retry", "event_id", event.EventID,
			"event_type", event.EventType, "err", txErr)
		return // 不 ACK：留在 PEL，等认领重投或进 DLQ
	}
	if duplicated {
		c.stats.Duplicated(event.EventType)
		slog.Info("duplicate event ignored", "event_id", event.EventID, "event_type", event.EventType)
	} else {
		c.stats.Processed(event.EventType)
	}
	c.ack(ctx, msg.ID)
}

func (c *Consumer) ack(ctx context.Context, id string) {
	if err := c.client.XAck(ctx, c.stream, c.group, id).Err(); err != nil {
		slog.Warn("xack failed", "id", id, "err", err)
	}
}

// deadLetter 把超过投递上限的消息投到 DLQ Stream 并 ACK 原消息：
// 原 Stream 不再积压，但事件本身没有丢——DLQ 里有完整载荷，可人工重放。
func (c *Consumer) deadLetter(ctx context.Context, msg redis.XMessage, deliveries int64) {
	eventType, _ := msg.Values["event_type"].(string)
	c.stats.DeadLettered(eventType)
	values := copyValues(msg.Values)
	values["dlq_reason"] = fmt.Sprintf("deliveries exceeded %d", c.maxDeliveries)
	values["dlq_deliveries"] = strconv.FormatInt(deliveries, 10)
	values["dlq_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := c.client.XAdd(ctx, &redis.XAddArgs{Stream: c.dlqStream, Values: values}).Err(); err != nil {
		slog.Error("move to DLQ failed, leaving message pending", "id", msg.ID, "err", err)
		return
	}
	slog.Error("event moved to DLQ", "id", msg.ID, "event_type", eventType, "deliveries", deliveries)
	c.ack(ctx, msg.ID)
}

func (c *Consumer) deadLetterRaw(ctx context.Context, msg redis.XMessage, reason string) {
	values := copyValues(msg.Values)
	values["dlq_reason"] = reason
	values["dlq_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := c.client.XAdd(ctx, &redis.XAddArgs{Stream: c.dlqStream, Values: values}).Err(); err != nil {
		slog.Error("move undecodable message to DLQ failed", "id", msg.ID, "err", err)
		return
	}
	c.ack(ctx, msg.ID)
}

func copyValues(values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

func (c *Consumer) reportLag(ctx context.Context) {
	groups, err := c.client.XInfoGroups(ctx, c.stream).Result()
	if err != nil {
		return
	}
	for _, group := range groups {
		if group.Name != c.group {
			continue
		}
		c.stats.Lag(group.Pending)
		if group.Pending > 0 {
			slog.Warn("outbox consumer lag", "group", group.Name, "pending", group.Pending, "lag", group.Lag)
		}
	}
}

// consumedEvent 对应 consumed_events 表（消费回执）。
type consumedEvent struct {
	Consumer    string    `gorm:"column:consumer;primaryKey"`
	EventID     uuid.UUID `gorm:"column:event_id;primaryKey"`
	EventType   string    `gorm:"column:event_type"`
	AggregateID int64     `gorm:"column:aggregate_id"`
	ConsumedAt  time.Time `gorm:"column:consumed_at"`
}

func (consumedEvent) TableName() string { return "consumed_events" }

func decodeMessage(msg redis.XMessage) (Event, error) {
	eventID, err := uuid.Parse(stringField(msg.Values, "event_id"))
	if err != nil {
		return Event{}, fmt.Errorf("invalid event_id: %w", err)
	}
	eventType := stringField(msg.Values, "event_type")
	if eventType == "" {
		return Event{}, errors.New("missing event_type")
	}
	aggregateID, err := strconv.ParseInt(stringField(msg.Values, "aggregate_id"), 10, 64)
	if err != nil {
		return Event{}, fmt.Errorf("invalid aggregate_id: %w", err)
	}
	payload := stringField(msg.Values, "payload")
	if payload == "" {
		return Event{}, errors.New("missing payload")
	}
	return Event{
		EventID:       eventID,
		EventType:     eventType,
		AggregateType: stringField(msg.Values, "aggregate_type"),
		AggregateID:   aggregateID,
		PartitionKey:  stringField(msg.Values, "partition_key"),
		Payload:       json.RawMessage(payload),
	}, nil
}

func stringField(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}
