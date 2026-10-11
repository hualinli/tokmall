package outbox_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/testsupport"
)

func TestStreamCapacityPreservesUnreadAndPending(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: testsupport.RedisAddr(t)})
	ctx := context.Background()
	stream := "test:retention:" + uuid.NewString()
	t.Cleanup(func() { _ = client.Del(ctx, stream, stream+":pressure").Err(); _ = client.Close() })
	p := outbox.NewRedisStreamPublisher(client, stream, 8, outbox.WithRequiredConsumerGroup("a"))
	msg := outbox.Message{EventID: uuid.NewString(), EventType: outbox.EventTypeOrderPaid, OccurredAt: time.Now()}
	require.ErrorIs(t, p.Publish(ctx, msg), outbox.ErrStreamBackpressure)
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, "other", "0").Err())
	require.ErrorIs(t, p.Publish(ctx, msg), outbox.ErrStreamBackpressure, "必须存在配置的消费组")
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, "a", "0").Err())
	var wg sync.WaitGroup
	errors := make(chan error, 64)
	for range 64 {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- p.Publish(ctx, msg) }()
	}
	wg.Wait()
	close(errors)
	success := 0
	for err := range errors {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, outbox.ErrStreamBackpressure)
		}
	}
	require.Equal(t, 8, success, "并发发布也不能超过容量预算")
	read := func(group string) []string {
		rows, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: group, Consumer: "reader", Streams: []string{stream, ">"}, Count: 8, Block: -1}).Result()
		require.NoError(t, err)
		ids := make([]string, 0, 8)
		for _, row := range rows[0].Messages {
			ids = append(ids, row.ID)
		}
		return ids
	}
	a := read("a")
	require.NoError(t, client.XAck(ctx, stream, "a", a...).Err())
	require.ErrorIs(t, p.Publish(ctx, msg), outbox.ErrStreamBackpressure, "不能删除另一组未读的消息")
	b := read("other")
	require.NoError(t, client.XAck(ctx, stream, "other", b[1:]...).Err())
	require.ErrorIs(t, p.Publish(ctx, msg), outbox.ErrStreamBackpressure, "乱序ACK不能越过最早待确认项")
	require.EqualValues(t, 8, client.XLen(ctx, stream).Val())
	require.NoError(t, client.XAck(ctx, stream, "other", b[0]).Err())
	require.NoError(t, p.Publish(ctx, msg))
	require.EqualValues(t, 1, client.XLen(ctx, stream).Val())
}

func TestSingleEntryBudgetCanResumeAfterAck(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: testsupport.RedisAddr(t)})
	ctx := context.Background()
	stream := "test:one:" + uuid.NewString()
	t.Cleanup(func() { _ = client.Del(ctx, stream, stream+":pressure").Err(); _ = client.Close() })
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, "group", "0").Err())
	p := outbox.NewRedisStreamPublisher(client, stream, 1)
	msg := outbox.Message{EventID: uuid.NewString(), OccurredAt: time.Now()}
	require.NoError(t, p.Publish(ctx, msg))
	require.ErrorIs(t, p.Publish(ctx, msg), outbox.ErrStreamBackpressure)
	rows, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: "group", Consumer: "reader", Streams: []string{stream, ">"}, Count: 1, Block: -1}).Result()
	require.NoError(t, err)
	require.NoError(t, client.XAck(ctx, stream, "group", rows[0].Messages[0].ID).Err())
	require.NoError(t, p.Publish(ctx, msg))
	require.EqualValues(t, 1, client.XLen(ctx, stream).Val())
}

func TestLeaseAndBackpressureDoNotExhaustRetries(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	ctx := context.Background()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return outbox.EnqueueOrderPaid(tx, outbox.OrderPaidPayload{OrderID: 1, PaidAt: time.Now()})
	}))
	repo := outbox.NewRepository(db)
	now := time.Now()
	first, err := repo.FetchPending(ctx, 1, time.Minute, now)
	require.NoError(t, err)
	require.Len(t, first, 1)
	second, err := outbox.NewRepository(db).FetchPending(ctx, 1, time.Minute, now)
	require.NoError(t, err)
	require.Empty(t, second, "有效租约不能被另一中继认领")
	require.NoError(t, repo.DeferPending(ctx, first, now))
	for range 10 {
		claimed, err := repo.FetchPending(ctx, 1, time.Minute, now)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.EqualValues(t, 1, claimed[0].Attempts)
		require.NoError(t, repo.DeferPending(ctx, claimed, now))
	}
	claimed, err := repo.FetchPending(ctx, 1, time.Second, now)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	newLease, err := repo.FetchPending(ctx, 1, time.Minute, now.Add(2*time.Second))
	require.NoError(t, err)
	require.Len(t, newLease, 1)
	require.NoError(t, repo.DeferPending(ctx, claimed, now))
	blocked, err := repo.FetchPending(ctx, 1, time.Minute, now.Add(3*time.Second))
	require.NoError(t, err)
	require.Empty(t, blocked, "旧持有者不能释放新租约")
}

func TestRelayBackpressureResumesWithoutDeadEvents(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	client := redis.NewClient(&redis.Options{Addr: testsupport.RedisAddr(t)})
	t.Cleanup(func() { _ = client.Close() })
	env := newTestEnv(t, db, client, "pressure-resume")
	t.Cleanup(func() { _ = client.Del(context.Background(), env.stream+":pressure").Err() })
	author := seedAuthor(t, db)
	for i := range 3 {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			return outbox.EnqueueOrderPaid(tx, outbox.OrderPaidPayload{OrderID: int64(i + 1), PaidAt: time.Now(), TotalAmount: 100, Units: 1, SourceAuthorID: &author})
		}))
	}
	// RunOnce不启动ticker；0延迟让测试不需睡眠即可反复触发背压。
	relay := outbox.NewRelay(env.repo, outbox.NewRedisStreamPublisher(client, env.stream, 1), 0, 10, time.Second, 2)
	for range 6 {
		_, err := relay.RunOnce(context.Background())
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, client.XLen(context.Background(), env.stream).Val())
	require.Zero(t, countRows(t, db, "outbox_events", "status='DEAD'"))
	require.EqualValues(t, 2, countRows(t, db, "outbox_events", "status='PENDING' AND attempts=0 AND leased_until IS NULL"))
	for range 3 {
		_, err := env.consumer.ConsumeOnce(context.Background())
		require.NoError(t, err)
		_, err = relay.RunOnce(context.Background())
		require.NoError(t, err)
	}
	require.EqualValues(t, 3, loadDaily(t, db, author).OrderCount)
	require.EqualValues(t, 3, countRows(t, db, "consumed_events", "true"))
}
