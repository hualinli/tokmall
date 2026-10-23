package outbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/testsupport"
)

// 对照「中继到 Redis Streams 再消费」和「消费者直接 SKIP LOCKED 取 outbox」。
// 负载是同一批已写入的事件被排空，不是 300 单/秒、30 分钟的开环压测。
// 它回答的是多一跳队列在中等批量下的耗时，不代替容量拐点实验。
//
//	TOKMALL_RUN_PG_INTEGRATION=1 TOKMALL_RUN_REDIS_INTEGRATION=1 \
//	TOKMALL_TEST_DATABASE_DSN=... TOKMALL_TEST_REDIS_ADDR=127.0.0.1:6379 \
//	go test ./internal/outbox -run TestOutboxTransportDrain -count=1 -v
func TestOutboxTransportDrain(t *testing.T) {
	dsn := testsupport.RequireDSN(t)
	redisAddr := testsupport.RedisAddr(t)
	const events = 500

	postgres := drainPostgres(t, dsn, events)
	streams := drainStreams(t, dsn, redisAddr, events)
	t.Logf("drain %d events: postgres=%s streams=%s", events, postgres, streams)
	require.Positive(t, postgres)
	require.Positive(t, streams)
}

type benchHandler struct{}

func (benchHandler) EventTypes() []string { return []string{"bench.noop.v1"} }

func (benchHandler) Handle(context.Context, *gorm.DB, outbox.Event) error { return nil }

func drainPostgres(t *testing.T, dsn string, n int) time.Duration {
	t.Helper()
	db := testsupport.IsolatedPostgres(t, dsn)
	insertBenchEvents(t, db, n)
	consumer := outbox.NewDirectConsumer(outbox.NewRepository(db), db, "bench-pg", []outbox.Handler{benchHandler{}}, time.Millisecond, 50, 30*time.Second, 5)
	start := time.Now()
	for {
		processed, err := consumer.RunOnce(context.Background())
		require.NoError(t, err)
		if processed == 0 {
			break
		}
	}
	elapsed := time.Since(start)
	require.Equal(t, int64(n), countPublished(t, db))
	require.Equal(t, int64(n), countReceipts(t, db, "bench-pg"))
	return elapsed
}

func drainStreams(t *testing.T, dsn, redisAddr string, n int) time.Duration {
	t.Helper()
	db := testsupport.IsolatedPostgres(t, dsn)
	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())
	stream := "tokmall:bench:" + uuid.NewString()
	t.Cleanup(func() { _ = client.Del(context.Background(), stream).Err() })
	insertBenchEvents(t, db, n)

	relay := outbox.NewRelay(outbox.NewRepository(db), outbox.NewRedisStreamPublisher(client, stream, 100000),
		time.Millisecond, 50, 30*time.Second, 5, outbox.WithRelayBackoff(0))
	consumer := outbox.NewConsumer(db, client, stream, "bench-streams", stream+":dlq", "bench",
		[]outbox.Handler{benchHandler{}}, outbox.WithConsumerBlock(20*time.Millisecond), outbox.WithConsumerBatch(50))
	require.NoError(t, consumer.EnsureGroup(context.Background()))

	start := time.Now()
	for {
		published, err := relay.RunOnce(context.Background())
		require.NoError(t, err)
		consumed, err := consumer.ConsumeOnce(context.Background())
		require.NoError(t, err)
		if published == 0 && consumed == 0 && countPublished(t, db) == int64(n) {
			break
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("streams drain stuck: published=%d consumed=%d done=%d", published, consumed, countPublished(t, db))
		}
	}
	elapsed := time.Since(start)
	require.Equal(t, int64(n), countReceipts(t, db, "bench-streams"))
	return elapsed
}

func insertBenchEvents(t *testing.T, db *gorm.DB, n int) {
	t.Helper()
	now := time.Now().UTC()
	events := make([]outbox.Event, n)
	for i := range events {
		events[i] = outbox.Event{
			EventID: uuid.New(), EventType: "bench.noop.v1", AggregateType: "bench",
			AggregateID: int64(i + 1), PartitionKey: "bench", Payload: []byte(`{}`),
			Status: outbox.StatusPending, AvailableAt: now, CreatedAt: now,
		}
	}
	require.NoError(t, db.CreateInBatches(events, 100).Error)
}

func countPublished(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM outbox_events WHERE status = 'PUBLISHED'`).Scan(&n).Error)
	return n
}

func countReceipts(t *testing.T, db *gorm.DB, consumer string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM consumed_events WHERE consumer = ?`, consumer).Scan(&n).Error)
	return n
}
