package reconcile_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/attribution"
	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/reconcile"
	"github.com/lihualin/tokmall/internal/testsupport"
)

func TestRebuildRejectsPendingEventsThenRemainsIdempotent(t *testing.T) {
	addr := testsupport.RedisAddr(t)
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	ctx := context.Background()
	author, _, _, _ := seedSalesFixture(t, db, false)
	const group = "attribution-projection"
	repo := reconcile.NewRepository(db)
	runner := reconcile.NewRunner(repo, reconcile.WithConsumer(group))
	var event outbox.Event
	require.NoError(t, db.Take(&event).Error)
	var paidAt time.Time
	require.NoError(t, db.Raw("SELECT paid_at FROM orders WHERE id = ?", event.AggregateID).Scan(&paidAt).Error)
	payload, err := json.Marshal(outbox.OrderPaidPayload{
		OrderID: event.AggregateID, SourceAuthorID: &author, TotalAmount: 200, Units: 1, PaidAt: paidAt,
	})
	require.NoError(t, err)
	require.NoError(t, db.Exec("UPDATE outbox_events SET payload = ?::jsonb WHERE event_id = ?", string(payload), event.EventID).Error)
	// 未消费的事实不能被重建预先计入，不论事件在 PG 处于何种投递状态。
	for _, status := range []string{"PENDING", "PUBLISHED", "DEAD"} {
		require.NoError(t, db.Exec("UPDATE outbox_events SET status = ?, published_at = CASE WHEN ? = 'PUBLISHED' THEN now() ELSE NULL END", status, status).Error)
		_, err = runner.RebuildProjection(ctx)
		require.ErrorIs(t, err, reconcile.ErrRebuildPendingEvents)
		var rows int64
		require.NoError(t, db.Table("author_sales_daily").Count(&rows).Error)
		require.Zero(t, rows)
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	stream := "rebuild:" + uuid.NewString()
	t.Cleanup(func() { _ = client.Del(ctx, stream, stream+":dlq").Err() })
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	consumer := outbox.NewConsumer(db, client, stream, group, stream+":dlq", "test",
		[]outbox.Handler{attribution.NewProjector(db)}, outbox.WithConsumerBlock(time.Millisecond))
	deliver := func() {
		t.Helper()
		require.NoError(t, client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{
			"event_id": event.EventID.String(), "event_type": event.EventType,
			"aggregate_id": event.AggregateID, "payload": string(payload),
		}}).Err())
		n, consumeErr := consumer.ConsumeOnce(ctx)
		require.NoError(t, consumeErr)
		require.Equal(t, 1, n)
	}
	deliver()
	// 消费完成后修复一次人为投影漂移，再重复重建和重放原事件。
	require.NoError(t, db.Exec("UPDATE author_sales_daily SET gmv = 999").Error)
	for range 2 {
		_, err = runner.RebuildProjection(ctx)
		require.NoError(t, err)
		deliver()
		var gmv int64
		require.NoError(t, db.Raw("SELECT sum(gmv) FROM author_sales_daily").Scan(&gmv).Error)
		require.Equal(t, int64(200), gmv, "重建与延迟/重复投递不能把 200 变成 400")
	}
}

func TestRebuildRequiresActualConsumerAndQuietWrites(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	seedCleanFixture(t, db)
	ctx := context.Background()
	repo := reconcile.NewRepository(db)
	_, err := repo.RebuildProjection(ctx, "")
	require.ErrorIs(t, err, reconcile.ErrRebuildConsumerRequired)
	_, err = repo.RebuildProjection(ctx, "another-group")
	require.ErrorIs(t, err, reconcile.ErrRebuildPendingEvents)
	for _, table := range []string{"orders", "order_items", "refunds", "outbox_events", "consumed_events", "author_sales_daily"} {
		t.Run(table, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			// 模拟活跃 writer 持有的表锁，验证 NOWAIT 拒绝而不是等待线上事务。
			require.NoError(t, tx.Exec("LOCK TABLE "+table+" IN ROW EXCLUSIVE MODE").Error)
			attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_, rebuildErr := repo.RebuildProjection(attemptCtx, "attribution-projection")
			require.ErrorIs(t, rebuildErr, reconcile.ErrRebuildBusy)
		})
	}
	_, err = repo.RebuildProjection(ctx, "attribution-projection")
	require.NoError(t, err)
	// 未来 timeout 是正常延迟任务，不参与销售投影的消费门禁。
	require.NoError(t, db.Exec(`INSERT INTO outbox_events
		(event_id, event_type, aggregate_type, aggregate_id, partition_key, payload, available_at)
		VALUES (?, ?, 'order', 1, 'order:1', '{}'::jsonb, now() + interval '1 day')`,
		uuid.New(), outbox.EventTypeOrderTimeout).Error)
	_, err = repo.RebuildProjection(ctx, "attribution-projection")
	require.NoError(t, err)
	// 已消费支付不能掩盖尚未消费的退款。
	require.NoError(t, db.Exec(`INSERT INTO outbox_events
		(event_id, event_type, aggregate_type, aggregate_id, partition_key, payload)
		VALUES (?, ?, 'order', 1, 'order:1', '{}'::jsonb)`,
		uuid.New(), outbox.EventTypeOrderRefunded).Error)
	_, err = repo.RebuildProjection(ctx, "attribution-projection")
	require.ErrorIs(t, err, reconcile.ErrRebuildPendingEvents)
	var gmv int64
	require.NoError(t, db.Raw("SELECT sum(gmv) FROM author_sales_daily").Scan(&gmv).Error)
	require.Equal(t, int64(200), gmv, "拒绝重建必须保留原投影")
}
