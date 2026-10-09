package reconcile_test

// 缺失回执检查的回归测试。
//
// 背景：consumed_events 的主键是 (consumer, event_id)，所以"有没有回执"本质上是
// **针对某个消费组**而言的：组 A 处理过的事件，对组 B 仍然是缺失的。早先的
// MissingReceipts 只按 event_id 关联、不带 consumer，于是任意一个组的回执都会
// 把其他组的缺失一并掩盖。多一个消费组（或改过 OUTBOX_CONSUMER_GROUP）之后，
// 对账对落后那个组完全失明——而它恰恰是发现问题的那个工具。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/reconcile"
	"github.com/lihualin/tokmall/internal/testsupport"
)

const (
	groupA = "tokmall-consumer"
	groupB = "tokmall-consumer-v2" // 模拟改名 / 第二个消费组
)

// publishEvent 写一条已投递（PUBLISHED）且 published_at 足够早的事件。
func publishEvent(t *testing.T, db *gorm.DB) string {
	t.Helper()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return outbox.EnqueueOrderTimeout(tx, outbox.OrderTimeoutPayload{
			OrderID: 1, OrderNo: "order-receipt", ExpiresAt: time.Now().UTC().Add(-time.Hour),
		})
	}))
	require.NoError(t, db.Exec(
		`UPDATE outbox_events SET status = 'PUBLISHED', published_at = now() - interval '1 hour'
		 WHERE aggregate_id = 1`).Error)

	var eventID string
	require.NoError(t, db.Raw(
		`SELECT event_id::text FROM outbox_events WHERE aggregate_id = 1 LIMIT 1`).Scan(&eventID).Error)
	require.NotEmpty(t, eventID)
	return eventID
}

func writeReceipt(t *testing.T, db *gorm.DB, consumer, eventID, eventType string) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO consumed_events (consumer, event_id, event_type, aggregate_id)
		 VALUES (?, ?::uuid, ?, 1) ON CONFLICT DO NOTHING`,
		consumer, eventID, eventType).Error)
}

func receiptCount(t *testing.T, db *gorm.DB, consumer string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM consumed_events WHERE consumer = ?`, consumer).Scan(&n).Error)
	return n
}

// TestMissingReceipts_IsScopedToConsumer 是核心一格：
// 组 A 的回执不得掩盖组 B 的缺失。
func TestMissingReceipts_IsScopedToConsumer(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	ctx := context.Background()
	repo := reconcile.NewRepository(db)

	eventID := publishEvent(t, db)
	t.Logf("事件 %s 已 PUBLISHED", eventID)

	t.Run("没有回执时，任何消费组都应看到缺失", func(t *testing.T) {
		for _, group := range []string{groupA, groupB} {
			missing, err := repo.MissingReceipts(ctx, group, time.Minute)
			require.NoError(t, err)
			require.Equal(t, []string{eventID}, missing, "消费组 %s 应看到缺失", group)
		}
	})

	t.Run("组 A 已有回执：只对组 A 不算缺失", func(t *testing.T) {
		writeReceipt(t, db, groupA, eventID, outbox.EventTypeOrderTimeout)

		missing, err := repo.MissingReceipts(ctx, groupA, time.Minute)
		require.NoError(t, err)
		require.Empty(t, missing, "组 A 已处理，不该报缺失")

		missing, err = repo.MissingReceipts(ctx, groupB, time.Minute)
		require.NoError(t, err)
		require.Equal(t, []string{eventID}, missing,
			"组 B 一条回执都没有，缺失必须对它可见——早先这里会被组 A 的回执掩盖")
		require.Zero(t, receiptCount(t, db, groupB), "前置条件：组 B 确实没有任何回执")
	})

	t.Run("consumer 为空时不限定消费组", func(t *testing.T) {
		missing, err := repo.MissingReceipts(ctx, "", time.Minute)
		require.NoError(t, err)
		require.Empty(t, missing,
			"空 consumer 保留旧的“任何组有回执即视为已消费”语义，仅供看全局情况")
	})
}

// TestRunner_ReplaysOnlyForItsOwnConsumer 确认修复贯通到 Runner：
// 只有自己这个组缺回执时才会触发重放——不重放别人组的事件。
func TestRunner_ReplaysOnlyForItsOwnConsumer(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	ctx := context.Background()

	eventID := publishEvent(t, db)
	writeReceipt(t, db, groupA, eventID, outbox.EventTypeOrderTimeout)

	// 针对组 B 跑：应当看到 1 条缺失并重放它。
	runnerB := reconcile.NewRunner(reconcile.NewRepository(db),
		reconcile.WithGrace(time.Minute), reconcile.WithConsumer(groupB))
	replayed, err := runnerB.ReplayMissingReceipts(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, replayed, "组 B 缺回执，应当重放")

	var status string
	require.NoError(t, db.Raw(`SELECT status FROM outbox_events WHERE event_id = ?`, eventID).Scan(&status).Error)
	require.Equal(t, "PENDING", status, "重放后必须回到待投递状态")

	// 事件已被组 B 重放、但仍未被消费；把它归位后，针对组 A 的 Runner 不应有动作。
	require.NoError(t, db.Exec(
		`UPDATE outbox_events SET status = 'PUBLISHED', published_at = now() - interval '1 hour'
		 WHERE event_id = ?`, eventID).Error)
	runnerA := reconcile.NewRunner(reconcile.NewRepository(db),
		reconcile.WithGrace(time.Minute), reconcile.WithConsumer(groupA))
	replayed, err = runnerA.ReplayMissingReceipts(ctx)
	require.NoError(t, err)
	require.Zero(t, replayed, "组 A 已有回执，不应触发任何重放")
}

var _ = gorm.ErrRecordNotFound
