package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/lihualin/tokmall/internal/attribution"
	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/testsupport"
)

// Outbox 的证据测试：证明"事件与业务同事务、至少一次投递、幂等消费、
// 失败重试与 DLQ 重放"这几条语义在真实 PostgreSQL + Redis 上成立。
//
//	TOKMALL_RUN_PG_INTEGRATION=1 \
//	TOKMALL_RUN_REDIS_INTEGRATION=1 \
//	TOKMALL_TEST_DATABASE_DSN='host=127.0.0.1 user=tokmall password=tokmall dbname=tokmall port=5432 sslmode=disable' \
//	go test ./internal/outbox -count=1 -v
func TestOutboxEndToEnd(t *testing.T) {
	dsn := testsupport.RequireDSN(t)
	redisAddr := testsupport.RedisAddr(t)
	db := testsupport.IsolatedPostgres(t, dsn)
	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	t.Run("paid event is projected exactly once", func(t *testing.T) {
		env := newTestEnv(t, db, client, "e2e-paid")
		authorID := seedAuthor(t, db)
		payload := outbox.OrderPaidPayload{
			OrderID: 1, OrderNo: "order-1", UserID: 2, MerchantID: 3,
			TotalAmount: 1999, Units: 2, PaidAt: time.Now().UTC(),
			SourceAuthorID: &authorID,
		}
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			return outbox.EnqueueOrderPaid(tx, payload)
		}))

		processed := env.drain(t)
		require.Equal(t, 1, processed, "一条事件应被消费一次")

		daily := loadDaily(t, db, authorID)
		require.Equal(t, int64(1), daily.OrderCount)
		require.Equal(t, int64(2), daily.UnitsSold)
		require.Equal(t, int64(1999), daily.GMV)

		// 中继把事件标记为 PUBLISHED，且消费回执只有一条。
		require.Equal(t, int64(1), countRows(t, db, "outbox_events", "status = 'PUBLISHED' AND aggregate_id = 1"))
		require.Equal(t, int64(1), countRows(t, db, "consumed_events", "event_type = 'commerce.order.paid.v1' AND aggregate_id = 1"))
	})

	t.Run("duplicate delivery does not double count", func(t *testing.T) {
		env := newTestEnv(t, db, client, "e2e-dup")
		authorID := seedAuthor(t, db)
		payload := outbox.OrderPaidPayload{
			OrderID: 7, OrderNo: "order-7", UserID: 2, MerchantID: 3,
			TotalAmount: 500, Units: 1, PaidAt: time.Now().UTC(), SourceAuthorID: &authorID,
		}
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			return outbox.EnqueueOrderPaid(tx, payload)
		}))
		env.drain(t)

		// 模拟"投递成功但标记失败"：同一条事件被重新投进流里（event_id 不变）。
		event := loadEvent(t, db, 1)
		require.NoError(t, env.publishRaw(t, event))
		// 消费组已经读过这条消息，重新投递会被当成重复事件忽略。
		env.drain(t)

		daily := loadDaily(t, db, authorID)
		require.Equal(t, int64(1), daily.OrderCount, "重复投递不得重复累加")
		require.Equal(t, int64(500), daily.GMV)
		require.Equal(t, int64(1), countRows(t, db, "consumed_events", "event_type = 'commerce.order.paid.v1' AND aggregate_id = 7"),
			"同一 event_id 只能有一条消费回执")
	})

	t.Run("refund event offsets the projection", func(t *testing.T) {
		env := newTestEnv(t, db, client, "e2e-refund")
		authorID := seedAuthor(t, db)
		now := time.Now().UTC()
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			if err := outbox.EnqueueOrderPaid(tx, outbox.OrderPaidPayload{
				OrderID: 11, OrderNo: "order-11", UserID: 2, MerchantID: 3,
				TotalAmount: 800, Units: 1, PaidAt: now, SourceAuthorID: &authorID,
			}); err != nil {
				return err
			}
			return outbox.EnqueueOrderRefunded(tx, outbox.OrderRefundedPayload{
				OrderID: 11, OrderNo: "order-11", RefundNo: "refund-11", UserID: 2, MerchantID: 3,
				Amount: 800, Units: 1, RestoreStock: true, RefundedAt: now, SourceAuthorID: &authorID,
			})
		}))
		require.Equal(t, 2, env.drain(t))

		daily := loadDaily(t, db, authorID)
		require.Equal(t, int64(1), daily.OrderCount)
		require.Equal(t, int64(800), daily.GMV)
		require.Equal(t, int64(1), daily.RefundedOrders)
		require.Equal(t, int64(800), daily.RefundedAmount, "退款金额必须被投影记录，用于净额对账")
	})

	t.Run("business rollback drops the event with it", func(t *testing.T) {
		// Transactional Outbox 的关键性质：事务回滚时事件也不存在，
		// 不会出现"业务没成功但下游收到通知"。
		env := newTestEnv(t, db, client, "e2e-rollback")
		authorID := seedAuthor(t, db)
		sentinel := errors.New("business failure after enqueue")
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := outbox.EnqueueOrderPaid(tx, outbox.OrderPaidPayload{
				OrderID: 21, OrderNo: "order-21", UserID: 2, MerchantID: 3,
				TotalAmount: 100, Units: 1, PaidAt: time.Now().UTC(), SourceAuthorID: &authorID,
			}); err != nil {
				return err
			}
			return sentinel
		})
		require.ErrorIs(t, err, sentinel)
		require.Zero(t, countRows(t, db, "outbox_events", "aggregate_id = 21"))
		require.Zero(t, env.drain(t))
		require.Zero(t, loadDailyOrZero(t, db, authorID).OrderCount)
	})

	t.Run("publish failure retries then dead letters and can be replayed", func(t *testing.T) {
		env := newTestEnv(t, db, client, "e2e-dlq")
		authorID := seedAuthor(t, db)
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			return outbox.EnqueueOrderPaid(tx, outbox.OrderPaidPayload{
				OrderID: 31, OrderNo: "order-31", UserID: 2, MerchantID: 3,
				TotalAmount: 300, Units: 1, PaidAt: time.Now().UTC(), SourceAuthorID: &authorID,
			})
		}))

		// 用"总是失败"的投递器模拟 broker 停机：事件必须留在 Outbox 里重试，
		// 而不是消失。maxTries=2 时第二次失败进入 DEAD。
		failing := &failingPublisher{err: errors.New("broker unavailable")}
		relay := outbox.NewRelay(env.repo, failing, time.Millisecond, 10, time.Second, 2,
			outbox.WithRelayBackoff(0))
		for range 2 {
			_, err := relay.RunOnce(context.Background())
			require.NoError(t, err)
		}
		var status outbox.Status
		var attempts int32
		require.NoError(t, db.Raw(`SELECT status, attempts FROM outbox_events WHERE aggregate_id = 31`).Row().Scan(&status, &attempts))
		require.Equal(t, outbox.StatusDead, status)
		require.Equal(t, int32(2), attempts)
		require.Zero(t, loadDailyOrZero(t, db, authorID).OrderCount, "未投递的事件不能影响投影")

		// 人工重放：DLQ → PENDING（event_id 不变）→ 正常中继投递 → 消费。
		eventID := loadEventByAggregate(t, db, 31).EventID.String()
		replayed, err := env.repo.Requeue(context.Background(), eventID, time.Now())
		require.NoError(t, err)
		require.True(t, replayed)
		require.Equal(t, 1, env.drain(t))
		daily := loadDaily(t, db, authorID)
		require.Equal(t, int64(1), daily.OrderCount)
		require.Equal(t, int64(300), daily.GMV)
	})

	t.Run("stale message is reclaimed after consumer crash", func(t *testing.T) {
		env := newTestEnv(t, db, client, "e2e-claim")
		authorID := seedAuthor(t, db)
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			return outbox.EnqueueOrderPaid(tx, outbox.OrderPaidPayload{
				OrderID: 41, OrderNo: "order-41", UserID: 2, MerchantID: 3,
				TotalAmount: 700, Units: 1, PaidAt: time.Now().UTC(), SourceAuthorID: &authorID,
			})
		}))

		// 先让中继把事件投进流，再由"另一个消费者"读走但不 ACK，然后崩溃。
		_, err := env.relay.RunOnce(context.Background())
		require.NoError(t, err)
		crashed := "crashed-consumer"
		streams, err := client.XReadGroup(context.Background(), &redis.XReadGroupArgs{
			Group: env.group, Consumer: crashed,
			Streams: []string{env.stream, ">"}, Count: 10, Block: time.Second,
		}).Result()
		require.NoError(t, err)
		require.Len(t, streams, 1)
		require.Len(t, streams[0].Messages, 1)

		// 新消费者用 0 idle 认领（生产配置是 30s，测试里缩短以便可验证）。
		env.consumer = outbox.NewConsumer(db, client, env.stream, env.group, env.dlq, "rescuer",
			[]outbox.Handler{attribution.NewProjector(db)},
			outbox.WithConsumerClaimIdle(0), outbox.WithConsumerBlock(10*time.Millisecond))
		processed := env.drain(t)
		require.Equal(t, 1, processed)
		require.Equal(t, int64(1), loadDaily(t, db, authorID).OrderCount)
	})
}

// testEnv 是一套隔离的"流 + 组 + 中继 + 消费者"。
type testEnv struct {
	db       *gorm.DB
	client   *redis.Client
	repo     *outbox.GormRepository
	relay    *outbox.Relay
	consumer *outbox.Consumer
	stream   string
	group    string
	dlq      string
}

func newTestEnv(t *testing.T, db *gorm.DB, client *redis.Client, label string) *testEnv {
	t.Helper()
	suffix := label + "-" + uuid.NewString()[:8]
	env := &testEnv{
		db: db, client: client, repo: outbox.NewRepository(db),
		stream: "test:events:" + suffix, group: "test-group-" + suffix,
		dlq: "test:events:" + suffix + ":dlq",
	}
	env.relay = outbox.NewRelay(env.repo, outbox.NewRedisStreamPublisher(client, env.stream, 1000),
		time.Millisecond, 10, time.Second, 5)
	env.consumer = outbox.NewConsumer(db, client, env.stream, env.group, env.dlq, "test-consumer",
		[]outbox.Handler{attribution.NewProjector(db)},
		outbox.WithConsumerClaimIdle(0), outbox.WithConsumerBlock(10*time.Millisecond))
	// 先建消费组，避免"先投递后建组"导致消息被当作历史消息跳过。
	require.NoError(t, client.XGroupCreateMkStream(context.Background(), env.stream, env.group, "0").Err())
	t.Cleanup(func() {
		ctx := context.Background()
		_ = client.Del(ctx, env.stream, env.dlq).Err()
	})
	return env
}

// drain 反复执行"投递一批 + 消费一批 + 认领超时消息"，直到没有新事件。
// 它模拟真实进程里中继与消费者两个 goroutine 的交替推进。
func (e *testEnv) drain(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	processed := 0
	for range 20 {
		if _, err := e.relay.RunOnce(ctx); err != nil {
			require.NoError(t, err)
		}
		count, err := e.consumer.ConsumeOnce(ctx)
		require.NoError(t, err)
		claimed, err := e.consumer.ClaimOnce(ctx)
		require.NoError(t, err)
		processed += count + claimed
		if count+claimed == 0 {
			break
		}
	}
	return processed
}

func (e *testEnv) publishRaw(t *testing.T, event outbox.Event) error {
	t.Helper()
	publisher := outbox.NewRedisStreamPublisher(e.client, e.stream, 1000)
	return publisher.Publish(context.Background(), outbox.Message{
		EventID: event.EventID.String(), EventType: event.EventType,
		AggregateType: event.AggregateType, AggregateID: event.AggregateID,
		PartitionKey: event.PartitionKey, Payload: event.Payload, OccurredAt: event.CreatedAt,
	})
}

type failingPublisher struct {
	mu  sync.Mutex
	err error
}

func (p *failingPublisher) Name() string { return "failing" }

func (p *failingPublisher) Publish(context.Context, outbox.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func seedAuthor(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	prefix := "outbox-" + uuid.NewString()[:8]
	var authorID int64
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash)
		VALUES (?, ?, 'hash') RETURNING id`, prefix+"@example.test", prefix).Scan(&authorID).Error)
	return authorID
}

func loadDaily(t *testing.T, db *gorm.DB, authorID int64) attribution.AuthorSalesDaily {
	t.Helper()
	row := loadDailyOrZero(t, db, authorID)
	require.Positive(t, row.OrderCount+row.RefundedOrders, "投影必须已写入")
	return row
}

func loadDailyOrZero(t *testing.T, db *gorm.DB, authorID int64) attribution.AuthorSalesDaily {
	t.Helper()
	var rows []attribution.AuthorSalesDaily
	require.NoError(t, db.Where("author_id = ?", authorID).Find(&rows).Error)
	var total attribution.AuthorSalesDaily
	for _, row := range rows {
		total.OrderCount += row.OrderCount
		total.UnitsSold += row.UnitsSold
		total.GMV += row.GMV
		total.RefundedOrders += row.RefundedOrders
		total.RefundedAmount += row.RefundedAmount
	}
	return total
}

func countRows(t *testing.T, db *gorm.DB, table, where string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", table, where)).Scan(&count).Error)
	return count
}

func loadEvent(t *testing.T, db *gorm.DB, id int64) outbox.Event {
	t.Helper()
	var event outbox.Event
	require.NoError(t, db.Where("id = ?", id).Take(&event).Error)
	return event
}

func loadEventByAggregate(t *testing.T, db *gorm.DB, aggregateID int64) outbox.Event {
	t.Helper()
	var event outbox.Event
	require.NoError(t, db.Where("aggregate_id = ?", aggregateID).Order("id DESC").Take(&event).Error)
	return event
}

var _ = json.Marshal
