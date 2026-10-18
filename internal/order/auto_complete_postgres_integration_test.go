package order

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 自动确认收货的真实 PostgreSQL 证据测试，开关与 TestPostgresRefundProof 相同：
//
//	TOKMALL_RUN_PG_INTEGRATION=1 TOKMALL_TEST_DATABASE_DSN=... \
//	go test ./internal/order -run TestPostgresAutoCompleteProof -count=1 -v
//
// 要证明：只完成到期的 FULFILLING 订单；退款申请期间不会被自动确认；
// 拒绝退款后截止时间至少顺延宽限期；与并发退款申请竞争时每笔订单只有一个赢家。
func TestPostgresAutoCompleteProof(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn, "integration switch is on, TOKMALL_TEST_DATABASE_DSN is required")
	t0 := time.Now().UTC().Truncate(time.Second)

	t.Run("completes only due orders and buyer confirm replays", func(t *testing.T) {
		db := newIsolatedPostgres(t, dsn)
		repo := NewRepository(db)
		svc, clock := autoCompleteService(repo, t0)
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-due", 2, 5)
		due := mustCreatePaidOrder(t, svc, buyers[0], skuID, "auto-due-a", 1)
		later := mustCreatePaidOrder(t, svc, buyers[1], skuID, "auto-due-b", 1)

		mustFulfillAt(t, svc, clock, t0, merchantID, due.OrderID)
		mustFulfillAt(t, svc, clock, t0.Add(30*time.Minute), merchantID, later.OrderID)

		completed, err := repo.AutoCompleteDue(context.Background(), t0.Add(time.Hour), 100)
		require.NoError(t, err)
		require.Equal(t, int64(1), completed)
		require.Equal(t, StatusCompleted, orderStatus(t, db, due.OrderID))
		require.Equal(t, StatusFulfilling, orderStatus(t, db, later.OrderID), "未到期订单不能被提前确认")

		*clock = t0.Add(time.Hour)
		replay, err := svc.Complete(context.Background(), buyers[0], due.OrderID)
		require.NoError(t, err)
		require.True(t, replay.Replayed, "系统已确认后买家再点确认是重放，不是错误")

		completed, err = repo.AutoCompleteDue(context.Background(), t0.Add(time.Hour), 100)
		require.NoError(t, err)
		require.Zero(t, completed, "已完成订单不会被重复处理")
	})

	t.Run("refund request pauses auto complete and rejection extends deadline", func(t *testing.T) {
		db := newIsolatedPostgres(t, dsn)
		repo := NewRepository(db)
		svc, clock := autoCompleteService(repo, t0)
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-refund", 2, 5)
		late := mustCreatePaidOrder(t, svc, buyers[0], skuID, "auto-refund-late", 1)
		early := mustCreatePaidOrder(t, svc, buyers[1], skuID, "auto-refund-early", 1)
		mustFulfillAt(t, svc, clock, t0, merchantID, late.OrderID)
		mustFulfillAt(t, svc, clock, t0, merchantID, early.OrderID)

		// 截止前申请、截止后才被拒绝：顺延到拒绝时刻 + 宽限期。
		*clock = t0.Add(50 * time.Minute)
		_, err := svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[0], OrderID: late.OrderID, Reason: "破损"})
		require.NoError(t, err)
		// 截止前申请并被拒绝：剩余时间仍多于宽限期，保持原截止时间。
		*clock = t0.Add(10 * time.Minute)
		_, err = svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[1], OrderID: early.OrderID, Reason: "想退"})
		require.NoError(t, err)
		*clock = t0.Add(20 * time.Minute)
		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: early.OrderID})
		require.NoError(t, err)
		require.True(t, autoCompleteAt(t, db, early.OrderID).Equal(t0.Add(time.Hour)))

		completed, err := repo.AutoCompleteDue(context.Background(), t0.Add(2*time.Hour), 100)
		require.NoError(t, err)
		require.Equal(t, int64(1), completed, "只有 early 到期；退款中的 late 不能被自动确认")
		require.Equal(t, StatusRefundRequested, orderStatus(t, db, late.OrderID))

		*clock = t0.Add(2 * time.Hour)
		rejected, err := svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: late.OrderID})
		require.NoError(t, err)
		require.Equal(t, StatusFulfilling, rejected.OrderStatus)
		require.True(t, autoCompleteAt(t, db, late.OrderID).Equal(t0.Add(2*time.Hour+30*time.Minute)))

		completed, err = repo.AutoCompleteDue(context.Background(), t0.Add(2*time.Hour+29*time.Minute), 100)
		require.NoError(t, err)
		require.Zero(t, completed, "宽限期内买家还能确认或再次申请")
		completed, err = repo.AutoCompleteDue(context.Background(), t0.Add(2*time.Hour+30*time.Minute), 100)
		require.NoError(t, err)
		require.Equal(t, int64(1), completed)
	})

	t.Run("concurrent refund requests and worker have one winner per order", func(t *testing.T) {
		db := newIsolatedPostgres(t, dsn)
		repo := NewRepository(db)
		svc, clock := autoCompleteService(repo, t0)
		const orders = 40
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-race", orders, orders)
		ids := make([]int64, orders)
		for i := range orders {
			created := mustCreatePaidOrder(t, svc, buyers[i], skuID, fmt.Sprintf("auto-race-%d", i), 1)
			mustFulfillAt(t, svc, clock, t0, merchantID, created.OrderID)
			ids[i] = created.OrderID
		}
		*clock = t0.Add(time.Hour)

		var wg sync.WaitGroup
		refundErrs := make([]error, orders)
		for i := range orders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, refundErrs[i] = svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[i], OrderID: ids[i], Reason: "竞争"})
			}()
		}
		workerErr := make(chan error, 1)
		go func() {
			for range 20 {
				if _, err := repo.AutoCompleteDue(context.Background(), t0.Add(time.Hour), 5); err != nil {
					workerErr <- err
					return
				}
			}
			workerErr <- nil
		}()
		wg.Wait()
		require.NoError(t, <-workerErr)
		_, err := repo.AutoCompleteDue(context.Background(), t0.Add(time.Hour), orders)
		require.NoError(t, err)

		for i, id := range ids {
			status := orderStatus(t, db, id)
			var active int64
			require.NoError(t, db.Raw(`SELECT count(*) FROM refunds WHERE order_id = ? AND status = 'REQUESTED'`, id).Scan(&active).Error)
			switch {
			case refundErrs[i] == nil:
				require.Equal(t, StatusRefundRequested, status)
				require.Equal(t, int64(1), active)
			case errors.Is(refundErrs[i], ErrRefundNotAllowed):
				require.Equal(t, StatusCompleted, status, "退款被拒只能是因为系统已先确认收货")
				require.Zero(t, active)
			default:
				require.NoError(t, refundErrs[i])
			}
		}
	})

	t.Run("database keeps deadline paired with shipment", func(t *testing.T) {
		db := newIsolatedPostgres(t, dsn)
		svc, clock := autoCompleteService(NewRepository(db), t0)
		buyers, merchantID, skuID := seedRefundFixture(t, db, "auto-check", 1, 1)
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "auto-check", 1)
		mustFulfillAt(t, svc, clock, t0, merchantID, created.OrderID)
		err := db.Exec(`UPDATE orders SET auto_complete_at = NULL WHERE id = ?`, created.OrderID).Error
		require.ErrorContains(t, err, "orders_auto_complete_check")
		err = db.Exec(`UPDATE orders SET auto_complete_at = fulfilling_at WHERE id = ?`, created.OrderID).Error
		require.ErrorContains(t, err, "orders_auto_complete_check")
	})
}

func autoCompleteService(repo *GormRepository, start time.Time) (*Service, *time.Time) {
	clock := start
	svc := refundService(repo)
	svc.now = func() time.Time { return clock }
	svc.SetAutoCompletePolicy(time.Hour, 30*time.Minute)
	return svc, &clock
}

func mustFulfillAt(t *testing.T, svc *Service, clock *time.Time, at time.Time, merchantID, orderID int64) {
	t.Helper()
	*clock = at
	_, err := svc.Fulfill(context.Background(), merchantID, orderID)
	require.NoError(t, err)
}

func orderStatus(t *testing.T, db *gorm.DB, orderID int64) Status {
	t.Helper()
	var status Status
	require.NoError(t, db.Raw(`SELECT status FROM orders WHERE id = ?`, orderID).Scan(&status).Error)
	return status
}

func autoCompleteAt(t *testing.T, db *gorm.DB, orderID int64) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, db.Raw(`SELECT auto_complete_at FROM orders WHERE id = ?`, orderID).Scan(&at).Error)
	return at
}
