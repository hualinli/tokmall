package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Message 是中继交给传输层的消息形态。它不包含数据库主键：投递层只关心
// "这条事件是什么、属于哪个分区、载荷是什么"，重试与租约是存储层的事。
type Message struct {
	EventID       string
	EventType     string
	AggregateType string
	AggregateID   int64
	PartitionKey  string
	Payload       json.RawMessage
	OccurredAt    time.Time
}

// Publisher 是传输抽象。默认实现是 Redis Streams（本项目已依赖 Redis，
// 零新增基础设施）；换成 Kafka 只需实现同一个接口——事件语义、
// 幂等键、DLQ 与重放流程都不需要改。
type Publisher interface {
	Publish(ctx context.Context, msg Message) error
	Name() string
}

// LogPublisher 只记录日志，用于本地调试与单元测试：它让"中继逻辑"
// 可以脱离任何 broker 被验证。
type LogPublisher struct{}

func (LogPublisher) Name() string { return "log" }

func (LogPublisher) Publish(_ context.Context, msg Message) error {
	slog.Info("outbox event published (log transport)",
		"event_id", msg.EventID, "event_type", msg.EventType, "aggregate_id", msg.AggregateID)
	return nil
}

// RedisStreamPublisher 把事件 XADD 到 Redis Stream。
//
// 为什么选 Redis Streams 而不是 Kafka（ADR-014）：
//   - 本项目已有 Redis，Streams 自带 consumer group、消息确认（XACK）、
//     待处理列表（PEL）与认领（XAUTOCLAIM），足以支撑至少一次投递与
//     "消费者崩溃后重投"；
//   - 量级是单机学习项目，Kafka 的分区吞吐与保留策略在这里是过剩能力；
//   - 代价要诚实记录：Redis 的持久化弱于 Kafka（AOF everysec 仍有秒级
//     丢失窗口），且没有跨机房复制。因此本项目不把"事件不丢"完全押在
//     broker 上，而是用 Outbox + 消费回执 + 对账重放兜底。
type RedisStreamPublisher struct {
	client *redis.Client
	stream string
	// maxLen 近似裁剪：Stream 无限增长会吃满内存，保留上限与业务重放窗口匹配。
	maxLen int64
}

func NewRedisStreamPublisher(client *redis.Client, stream string, maxLen int64) *RedisStreamPublisher {
	return &RedisStreamPublisher{client: client, stream: stream, maxLen: maxLen}
}

func (p *RedisStreamPublisher) Name() string { return "redis-stream" }

func (p *RedisStreamPublisher) Publish(ctx context.Context, msg Message) error {
	values := map[string]any{
		"event_id":       msg.EventID,
		"event_type":     msg.EventType,
		"aggregate_type": msg.AggregateType,
		"aggregate_id":   msg.AggregateID,
		"partition_key":  msg.PartitionKey,
		"payload":        string(msg.Payload),
		"occurred_at":    msg.OccurredAt.UTC().Format(time.RFC3339Nano),
	}
	err := p.client.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		MaxLen: p.maxLen,
		Approx: true,
		Values: values,
	}).Err()
	if err != nil {
		return fmt.Errorf("xadd %s: %w", p.stream, err)
	}
	return nil
}
