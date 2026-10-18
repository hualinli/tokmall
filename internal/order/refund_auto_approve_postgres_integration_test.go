package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/reconcile"
)

// 到期自动同意必须复用 approveRefund，并且和商家裁决抢的是同一把订单锁。
//
//	TOKMALL_RUN_PG_INTEGRATION=1 \
//	TOKMALL_TEST_DATABASE_DSN='postgres://tokmall:tokmall@127.0.0.1:5432/tokmall?sslmode=disable' \
//	go test ./internal/order -run TestPostgresRefundAutoApprove -count=1 -v
func TestPostgresRefundAutoApprove(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := os.Getenv("TOKMALL_TEST_DATABASE_DSN")
	require.NotEmpty(t, dsn)
	db := newIsolatedPostgres(t, dsn)

	t.Run("only due refunds are approved as timeout", func(t *testing.T) {
		buyers, _, skuID := seedRefundFixture(t, db, "auto-due", 2, 4)
		noteOpeningStock(t, db, skuID, 4)
		repo := NewRepository(db)
		svc := refundService(repo)
		early := mustRequestRefund(t, svc, buyers[0], skuID, "auto-due-early")
		later := mustRequestRefund(t, svc, buyers[1], skuID, "auto-due-later")
		earlyAt := setRespondBy(t, db, early.OrderID, time.Hour)
		laterAt := setRespondBy(t, db, later.OrderID, 2*time.Hour)

		approved, err := repo.ApproveOverdueRefund(context.Background(), earlyAt.Add(-time.Second))
		require.NoError(t, err)
		require.False(t, approved)
		requireRefundStatus(t, db, early.OrderID, RefundStatusRequested, "")

		approved, err = repo.ApproveOverdueRefund(context.Background(), earlyAt)
		require.NoError(t, err)
		require.True(t, approved)
		requireTimeoutRefund(t, db, early.OrderID)
		requireRefundStatus(t, db, later.OrderID, RefundStatusRequested, "")

		approved, err = repo.ApproveOverdueRefund(context.Background(), earlyAt)
		require.NoError(t, err)
		require.False(t, approved, "未到期的退款单不能被同一时刻的下一次扫描带走")

		approved, err = repo.ApproveOverdueRefund(context.Background(), laterAt)
		require.NoError(t, err)
		require.True(t, approved)
		requireTimeoutRefund(t, db, later.OrderID)
	})

	t.Run("paid refund restores stock coupon and writes one event", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-paid", 1, 5)
		noteOpeningStock(t, db, skuID, 5)
		userCouponID := seedUserCoupon(t, db, buyers[0], merchantID, 1000, 500)
		repo := NewRepository(db)
		svc := refundService(repo)
		created, err := svc.Create(context.Background(), CreateParams{
			UserID: buyers[0], SKUID: skuID, Quantity: 2,
			IdempotencyKey: "auto-paid", UserCouponID: &userCouponID,
		})
		require.NoError(t, err)
		_, err = svc.Pay(context.Background(), PayParams{
			UserID: buyers[0], OrderID: created.OrderID,
			PaymentNo: "auto-paid-pay", ProviderTradeNo: "auto-paid-trade", Amount: created.TotalAmount,
		})
		require.NoError(t, err)
		_, err = svc.RequestRefund(context.Background(), RequestRefundParams{
			UserID: buyers[0], OrderID: created.OrderID, Reason: "到期退",
		})
		require.NoError(t, err)
		// 裁决时间就是 respond_by。券的有效期只有一小时，截止时间必须落在有效期内，退回才是 UNUSED。
		due := setRespondBy(t, db, created.OrderID, time.Minute)
		approved, err := repo.ApproveOverdueRefund(context.Background(), due)
		require.NoError(t, err)
		require.True(t, approved)
		requireTimeoutRefund(t, db, created.OrderID)

		var stock, ledgers int64
		require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
		require.Equal(t, int64(5), stock, "未发货退款必须把库存补回去")
		require.NoError(t, db.Raw(`SELECT count(*) FROM inventory_transactions
			WHERE order_id = ? AND reason = 'ORDER_REFUNDED'`, created.OrderID).Scan(&ledgers).Error)
		require.Equal(t, int64(1), ledgers)

		var couponStatus string
		var couponOrderID *int64
		require.NoError(t, db.Raw(`SELECT status, order_id FROM user_coupons WHERE id = ?`, userCouponID).
			Row().Scan(&couponStatus, &couponOrderID))
		require.Equal(t, "UNUSED", couponStatus)
		require.Nil(t, couponOrderID)

		var events int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM outbox_events
			WHERE aggregate_id = ? AND event_type = ?`, created.OrderID, outbox.EventTypeOrderRefunded).Scan(&events).Error)
		require.Equal(t, int64(1), events)
	})

	t.Run("shipped refund does not restore stock", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-shipped", 1, 3)
		noteOpeningStock(t, db, skuID, 3)
		repo := NewRepository(db)
		svc := refundService(repo)
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "auto-shipped", 1)
		_, err := svc.Fulfill(context.Background(), merchantID, created.OrderID)
		require.NoError(t, err)
		_, err = svc.RequestRefund(context.Background(), RequestRefundParams{
			UserID: buyers[0], OrderID: created.OrderID, Reason: "已发货",
		})
		require.NoError(t, err)
		due := setRespondBy(t, db, created.OrderID, time.Hour)
		approved, err := repo.ApproveOverdueRefund(context.Background(), due)
		require.NoError(t, err)
		require.True(t, approved)
		requireTimeoutRefund(t, db, created.OrderID)

		var stock, ledgers int64
		require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
		require.Equal(t, int64(2), stock, "已发货退款不回补库存")
		require.NoError(t, db.Raw(`SELECT count(*) FROM inventory_transactions
			WHERE order_id = ? AND reason = 'ORDER_REFUNDED'`, created.OrderID).Scan(&ledgers).Error)
		require.Zero(t, ledgers)
	})

	t.Run("skip locked order and approve the next one", func(t *testing.T) {
		buyers, _, skuID := seedRefundFixture(t, db, "auto-skip", 2, 2)
		noteOpeningStock(t, db, skuID, 2)
		repo := NewRepository(db)
		svc := refundService(repo)
		held := mustRequestRefund(t, svc, buyers[0], skuID, "auto-skip-held")
		free := mustRequestRefund(t, svc, buyers[1], skuID, "auto-skip-free")
		due := setRespondBy(t, db, held.OrderID, time.Hour)
		require.NoError(t, db.Exec(`UPDATE refunds SET respond_by = ? WHERE order_id = ?`, due, free.OrderID).Error)

		sqlDB, err := db.DB()
		require.NoError(t, err)
		tx, err := sqlDB.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		var locked int64
		require.NoError(t, tx.QueryRow(`SELECT id FROM orders WHERE id = $1 FOR UPDATE`, held.OrderID).Scan(&locked))

		approved, err := repo.ApproveOverdueRefund(context.Background(), due)
		require.NoError(t, err)
		require.True(t, approved, "被锁的订单应被跳过，下一笔到期退款仍要处理")
		requireTimeoutRefund(t, db, free.OrderID)
		requireRefundStatus(t, db, held.OrderID, RefundStatusRequested, "")

		approved, err = repo.ApproveOverdueRefund(context.Background(), due)
		require.NoError(t, err)
		require.False(t, approved, "只剩被锁的那一笔时，这一轮应当空手返回")
		require.NoError(t, tx.Rollback())

		approved, err = repo.ApproveOverdueRefund(context.Background(), due)
		require.NoError(t, err)
		require.True(t, approved)
		requireTimeoutRefund(t, db, held.OrderID)
	})

	t.Run("merchant and worker leave each order in one legal state", func(t *testing.T) {
		const orders = 8
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-race", 1, orders)
		noteOpeningStock(t, db, skuID, orders)
		repo := NewRepository(db)
		svc := refundService(repo)
		ids := make([]int64, 0, orders)
		var due time.Time
		for i := range orders {
			created := mustRequestRefund(t, svc, buyers[0], skuID, fmt.Sprintf("auto-race-%d", i))
			at := setRespondBy(t, db, created.OrderID, time.Hour)
			if due.IsZero() || at.After(due) {
				due = at
			}
			ids = append(ids, created.OrderID)
		}

		start := make(chan struct{})
		errCh := make(chan error, orders+4)
		var wg sync.WaitGroup
		for i, orderID := range ids {
			wg.Add(1)
			approve := i%2 == 0
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := svc.DecideRefund(ctx, DecideRefundParams{
					MerchantID: merchantID, OrderID: orderID, Approve: approve, Remark: "商家",
				})
				if !approve && errors.Is(err, ErrRefundAlreadyDecided) {
					err = nil
				}
				errCh <- err
			}()
		}
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for {
					ok, err := repo.ApproveOverdueRefund(context.Background(), due)
					if err != nil || !ok {
						errCh <- err
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			require.NoError(t, err)
		}

		var refunded int
		for _, orderID := range ids {
			var orderStatus Status
			var refundStatus RefundStatus
			var source string
			var decidedBy *int64
			require.NoError(t, db.Raw(`SELECT o.status, r.status, r.decision_source, r.decided_by
				FROM orders o JOIN refunds r ON r.order_id = o.id
				WHERE o.id = ? ORDER BY r.id DESC LIMIT 1`, orderID).Row().
				Scan(&orderStatus, &refundStatus, &source, &decidedBy))
			switch orderStatus {
			case StatusRefunded:
				require.Equal(t, RefundStatusSucceeded, refundStatus)
				if source == string(decisionSourceTimeout) {
					require.Nil(t, decidedBy)
				} else {
					require.Equal(t, string(decisionSourceMerchant), source)
					require.NotNil(t, decidedBy)
					require.Equal(t, merchantID, *decidedBy)
				}
				refunded++
			case StatusPaid:
				require.Equal(t, RefundStatusRejected, refundStatus)
				require.Equal(t, string(decisionSourceMerchant), source)
				require.NotNil(t, decidedBy)
				require.Equal(t, merchantID, *decidedBy)
			default:
				t.Fatalf("order %d ended in %s", orderID, orderStatus)
			}
		}
		var stock int64
		require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
		require.Equal(t, int64(refunded), stock, "只有真正退成的订单回补库存")
	})

	t.Run("late merchant approve replays and reject conflicts", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-late", 1, 2)
		noteOpeningStock(t, db, skuID, 2)
		repo := NewRepository(db)
		svc := refundService(repo)
		first := mustRequestRefund(t, svc, buyers[0], skuID, "auto-late-approve")
		second := mustRequestRefund(t, svc, buyers[0], skuID, "auto-late-reject")
		due := setRespondBy(t, db, first.OrderID, time.Hour)
		require.NoError(t, db.Exec(`UPDATE refunds SET respond_by = ? WHERE order_id = ?`, due, second.OrderID).Error)
		require.NoError(t, approveUntilDone(t, repo, due))

		replayed, err := svc.DecideRefund(context.Background(), DecideRefundParams{
			MerchantID: merchantID, OrderID: first.OrderID, Approve: true, Remark: "晚到的同意",
		})
		require.NoError(t, err)
		require.True(t, replayed.Replayed)
		requireTimeoutRefund(t, db, first.OrderID)

		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{
			MerchantID: merchantID, OrderID: second.OrderID, Remark: "晚到的拒绝",
		})
		require.ErrorIs(t, err, ErrRefundAlreadyDecided)
		requireTimeoutRefund(t, db, second.OrderID)
	})

	t.Run("re-request uses the new deadline", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-reapply", 1, 2)
		noteOpeningStock(t, db, skuID, 2)
		repo := NewRepository(db)
		svc := refundService(repo)
		created := mustRequestRefund(t, svc, buyers[0], skuID, "auto-reapply")
		oldDue := setRespondBy(t, db, created.OrderID, time.Hour)
		_, err := svc.DecideRefund(context.Background(), DecideRefundParams{
			MerchantID: merchantID, OrderID: created.OrderID, Remark: "先拒绝",
		})
		require.NoError(t, err)

		approved, err := repo.ApproveOverdueRefund(context.Background(), oldDue)
		require.NoError(t, err)
		require.False(t, approved, "被拒绝的旧单即使到期也不能自动同意")
		requireRefundStatus(t, db, created.OrderID, RefundStatusRejected, string(decisionSourceMerchant))

		_, err = svc.RequestRefund(context.Background(), RequestRefundParams{
			UserID: buyers[0], OrderID: created.OrderID, Reason: "再次申请",
		})
		require.NoError(t, err)
		var newDue time.Time
		require.NoError(t, db.Raw(`SELECT respond_by FROM refunds
			WHERE order_id = ? AND status = 'REQUESTED'`, created.OrderID).Scan(&newDue).Error)
		require.True(t, newDue.After(oldDue))

		approved, err = repo.ApproveOverdueRefund(context.Background(), oldDue)
		require.NoError(t, err)
		require.False(t, approved)
		approved, err = repo.ApproveOverdueRefund(context.Background(), newDue)
		require.NoError(t, err)
		require.True(t, approved)
		requireTimeoutRefund(t, db, created.OrderID)
		var rejected int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM refunds
			WHERE order_id = ? AND status = 'REJECTED' AND decision_source = 'MERCHANT'`, created.OrderID).Scan(&rejected).Error)
		require.Equal(t, int64(1), rejected)
	})

	t.Run("reconcile accepts the timeout refunds", func(t *testing.T) {
		report, err := reconcile.NewRunner(reconcile.NewRepository(db), reconcile.WithGrace(time.Minute)).Run(context.Background())
		require.NoError(t, err)
		for _, check := range report.Checks {
			require.Empty(t, check.Differences, check.Name)
		}
	})
}

func mustRequestRefund(t *testing.T, svc *Service, buyerID, skuID int64, key string) *CreateResult {
	t.Helper()
	created := mustCreatePaidOrder(t, svc, buyerID, skuID, key, 1)
	_, err := svc.RequestRefund(context.Background(), RequestRefundParams{
		UserID: buyerID, OrderID: created.OrderID, Reason: "自动同意",
	})
	require.NoError(t, err)
	return created
}

func setRespondBy(t *testing.T, db *gorm.DB, orderID int64, after time.Duration) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, db.Raw(`UPDATE refunds
		SET respond_by = requested_at + (? * interval '1 second')
		WHERE order_id = ? AND status = 'REQUESTED'
		RETURNING respond_by`, int64(after.Seconds()), orderID).Scan(&at).Error)
	require.False(t, at.IsZero())
	return at
}

func noteOpeningStock(t *testing.T, db *gorm.DB, skuID int64, quantity int) {
	t.Helper()
	require.NoError(t, db.Exec(`INSERT INTO inventory_transactions (sku_id, change, reason, biz_id)
		VALUES (?, ?, 'ADJUST', ?)`, skuID, quantity, "opening-"+uuid.NewString()).Error)
}

func approveUntilDone(t *testing.T, repo *GormRepository, now time.Time) error {
	t.Helper()
	for range 10 {
		ok, err := repo.ApproveOverdueRefund(context.Background(), now)
		if err != nil || !ok {
			return err
		}
	}
	t.Fatal("approve loop did not stop")
	return nil
}

func requireTimeoutRefund(t *testing.T, db *gorm.DB, orderID int64) {
	t.Helper()
	var orderStatus Status
	var refundStatus RefundStatus
	var source string
	var decidedBy *int64
	require.NoError(t, db.Raw(`SELECT o.status, r.status, r.decision_source, r.decided_by
		FROM orders o JOIN refunds r ON r.order_id = o.id
		WHERE o.id = ? AND r.status = 'SUCCEEDED'`, orderID).Row().
		Scan(&orderStatus, &refundStatus, &source, &decidedBy))
	require.Equal(t, StatusRefunded, orderStatus)
	require.Equal(t, RefundStatusSucceeded, refundStatus)
	require.Equal(t, string(decisionSourceTimeout), source)
	require.Nil(t, decidedBy)
}

func requireRefundStatus(t *testing.T, db *gorm.DB, orderID int64, status RefundStatus, source string) {
	t.Helper()
	var got RefundStatus
	var gotSource sql.NullString
	require.NoError(t, db.Raw(`SELECT status, decision_source FROM refunds
		WHERE order_id = ? ORDER BY id DESC LIMIT 1`, orderID).Row().Scan(&got, &gotSource))
	require.Equal(t, status, got)
	if source == "" {
		require.False(t, gotSource.Valid)
		return
	}
	require.Equal(t, source, gotSource.String)
}
