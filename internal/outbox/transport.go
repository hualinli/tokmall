package outbox

import (
	"context"
	"encoding/json"
	"errors"
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
	// maxLen 是缓冲预算；未消费消息占满容量时暂停投递。
	maxLen int64
	group  string
}

type StreamPublisherOption func(*RedisStreamPublisher)

func WithRequiredConsumerGroup(group string) StreamPublisherOption {
	return func(p *RedisStreamPublisher) { p.group = group }
}

func NewRedisStreamPublisher(client *redis.Client, stream string, maxLen int64, opts ...StreamPublisherOption) *RedisStreamPublisher {
	p := &RedisStreamPublisher{client: client, stream: stream, maxLen: maxLen}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

var ErrStreamBackpressure = errors.New("outbox stream is waiting for consumer capacity")

// Lua 原子读取全部组水位、裁剪和追加；同时约束多个发布者。
// 比较ID用十进制字符串，避免Lua浮点数把64位序号舍入。
var safeStreamAppend = redis.NewScript(`
local function less(a, b)
  local am, as = string.match(a, '^(%d+)%-(%d+)$')
  local bm, bs = string.match(b, '^(%d+)%-(%d+)$')
  if #am ~= #bm then return #am < #bm end
  if am ~= bm then return am < bm end
  if #as ~= #bs then return #as < #bs end
  return as < bs
end
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
local groups = redis.call('XINFO', 'GROUPS', KEYS[1])
if #groups == 0 then return 0 end
local required = ARGV[1] == ''
local length = redis.call('XLEN', KEYS[1])
local maximum = tonumber(ARGV[2])
local trim = redis.call('EXISTS', KEYS[2]) == 1 or length >= maximum
local boundary = nil
local inclusive = true
for _, g in ipairs(groups) do
  local info = {}
  for i = 1, #g, 2 do info[g[i]] = g[i+1] end
  if info['name'] == ARGV[1] then required = true end
  if trim then
    local pending = redis.call('XPENDING', KEYS[1], info['name'])
    local water = info['last-delivered-id']
    local acked = true
    if pending[1] > 0 then water = pending[2]; acked = false end
    if boundary == nil or less(water, boundary) then
      boundary = water; inclusive = acked
    elseif water == boundary then inclusive = inclusive and acked end
  end
end
if not required then return 0 end
if trim then
  redis.call('XTRIM', KEYS[1], 'MINID', '=', boundary)
  if inclusive then redis.call('XDEL', KEYS[1], boundary) end
  length = redis.call('XLEN', KEYS[1])
  if length >= math.max(1, math.floor(maximum / 2)) then
    redis.call('SET', KEYS[2], '1', 'EX', 60)
    return 0
  end
  redis.call('DEL', KEYS[2])
end
redis.call('XADD', KEYS[1], '*', unpack(ARGV, 3))
return 1
`)

func (p *RedisStreamPublisher) Name() string { return "redis-stream" }

func (p *RedisStreamPublisher) Publish(ctx context.Context, msg Message) error {
	args := []any{p.group, p.maxLen,
		"event_id", msg.EventID, "event_type", msg.EventType,
		"aggregate_type", msg.AggregateType, "aggregate_id", msg.AggregateID,
		"partition_key", msg.PartitionKey, "payload", string(msg.Payload),
		"occurred_at", msg.OccurredAt.UTC().Format(time.RFC3339Nano),
	}
	added, err := safeStreamAppend.Run(ctx, p.client, []string{p.stream, p.stream + ":pressure"}, args...).Int()
	if err != nil {
		return fmt.Errorf("xadd %s: %w", p.stream, err)
	}
	if added == 0 {
		return ErrStreamBackpressure
	}
	return nil
}
