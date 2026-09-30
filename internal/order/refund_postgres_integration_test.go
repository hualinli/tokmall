package order

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 售后（退款）的真实 PostgreSQL 证据测试。
// 普通 go test 不连接数据库；显式开启方式与 postgres_integration_test.go 相同：
//
//	TOKMALL_RUN_PG_INTEGRATION=1 \
//	TOKMALL_TEST_DATABASE_DSN='host=127.0.0.1 user=tokmall password=tokmall dbname=tokmall port=5432 sslmode=disable' \
//	go test ./internal/order -run TestPostgresRefundProof -count=1 -v
//
// 这里要证明的不是"代码看起来对"，而是三类不变量在真实并发下成立：
//  1. 库存守恒：期初 - 下单 + 退款回补 = 当前库存，且回补流水恰好一条；
//  2. 单一终态：申请/裁决/发货/收货竞争后，订单只会停在唯一合法状态；
//  3. 售后事实唯一：同一订单不会产生两笔并行或两笔成功的退款。
func TestPostgresRefundProof(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn, "integration switch is on, TOKMALL_TEST_DATABASE_DSN is required")
	db := newIsolatedPostgres(t, dsn)

	t.Run("approve refund restores stock exactly once", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "refund-once", 1, 5)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-once", 2)

		view, err := svc.RequestRefund(context.Background(), RequestRefundParams{
			UserID: buyers[0], OrderID: created.OrderID, Reason: "不想要了",
		})
		require.NoError(t, err)
		require.Equal(t, RefundStatusRequested, view.Status)
		require.True(t, view.RestoreStock)

		// 50 个并发"同意"必须只有一次真正执行退款，其余是幂等重放。
		var executed atomic.Int64
		runConcurrent(t, 50, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result, err := svc.DecideRefund(ctx, DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: true, Remark: "同意"})
			if err != nil {
				return err
			}
			if result.Status != RefundStatusSucceeded {
				return fmt.Errorf("unexpected refund status %s", result.Status)
			}
			if !result.Replayed {
				executed.Add(1)
			}
			return nil
		})
		require.Equal(t, int64(1), executed.Load(), "并发同意只应有一次真正执行")

		assertRefundFacts(t, db, created.OrderID, skuID, refundExpectation{
			initialStock:     5,
			orderStatus:      StatusRefunded,
			stock:            5,
			restoredLedgers:  1,
			refundRows:       1,
			successfulRefund: 1,
		})
	})

	t.Run("shipped refund keeps stock deducted", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "refund-shipped", 1, 3)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-shipped", 1)
		_, err := svc.Fulfill(context.Background(), merchantID, created.OrderID)
		require.NoError(t, err)

		view, err := svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "已发货但想退"})
		require.NoError(t, err)
		require.False(t, view.RestoreStock, "已发货退款不回补库存")

		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: true})
		require.NoError(t, err)

		assertRefundFacts(t, db, created.OrderID, skuID, refundExpectation{
			initialStock:     3,
			orderStatus:      StatusRefunded,
			stock:            2, // 货已出库，库存不回补
			restoredLedgers:  0,
			refundRows:       1,
			successfulRefund: 1,
		})
	})

	t.Run("reject then re-request restores stock once", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "refund-reject", 1, 4)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-reject", 1)

		_, err := svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "第一次"})
		require.NoError(t, err)
		rejected, err := svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Remark: "不同意"})
		require.NoError(t, err)
		require.Equal(t, RefundStatusRejected, rejected.Status)
		require.Equal(t, StatusPaid, rejected.OrderStatus, "拒绝后订单退回 PAID")

		// 拒绝后可以再次申请；此时订单仍是未发货状态，因此这次退款要回补库存。
		second, err := svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "第二次"})
		require.NoError(t, err)
		require.True(t, second.RestoreStock)
		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: true})
		require.NoError(t, err)

		assertRefundFacts(t, db, created.OrderID, skuID, refundExpectation{
			initialStock:     4,
			orderStatus:      StatusRefunded,
			stock:            4,
			restoredLedgers:  1,
			refundRows:       2, // 被拒的那笔 + 成功的那笔
			successfulRefund: 1,
		})
	})

	t.Run("refund request and fulfillment serialize on order state", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "refund-fulfill", 1, 2)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-fulfill", 1)

		start := make(chan struct{})
		refundResult := make(chan *RefundView, 1)
		refundErr := make(chan error, 1)
		fulfillErr := make(chan error, 1)
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			view, err := svc.RequestRefund(ctx, RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "竞争"})
			refundResult <- view
			refundErr <- err
		}()
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := svc.Fulfill(ctx, merchantID, created.OrderID)
			fulfillErr <- err
		}()
		close(start)
		view, refundFailure, fulfillFailure := <-refundResult, <-refundErr, <-fulfillErr
		require.NoError(t, refundFailure, "退款申请在 PAID 与 FULFILLING 下都合法，不应失败")
		require.NotNil(t, view)

		var status Status
		require.NoError(t, db.Raw(`SELECT status FROM orders WHERE id = ?`, created.OrderID).Scan(&status).Error)
		require.Equal(t, StatusRefundRequested, status, "无论谁先赢，订单都必须停在退款中")

		if fulfillFailure == nil {
			// 发货先赢：退款申请落在 FULFILLING 上，按"已发货退款"处理——不回补库存。
			require.False(t, view.RestoreStock, "发货已提交后的退款申请不得回补库存")
		} else {
			// 退款先赢：发货必须被订单状态机挡住。
			require.ErrorIs(t, fulfillFailure, ErrOrderStateConflict)
			require.True(t, view.RestoreStock)
		}

		// 退款中再次发货/确认收货必须失败：这是"退款锁死履约"的直接证据。
		_, err := svc.Fulfill(context.Background(), merchantID, created.OrderID)
		require.ErrorIs(t, err, ErrOrderStateConflict)
		_, err = svc.Complete(context.Background(), buyers[0], created.OrderID)
		require.ErrorIs(t, err, ErrOrderStateConflict)
	})

	t.Run("refund request and completion choose one winner", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "refund-complete", 1, 2)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-complete", 1)
		_, err := svc.Fulfill(context.Background(), merchantID, created.OrderID)
		require.NoError(t, err)

		start := make(chan struct{})
		refundErr := make(chan error, 1)
		completeErr := make(chan error, 1)
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := svc.RequestRefund(ctx, RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "竞争"})
			refundErr <- err
		}()
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := svc.Complete(ctx, buyers[0], created.OrderID)
			completeErr <- err
		}()
		close(start)
		first, second := <-refundErr, <-completeErr
		require.True(t, (first == nil) != (second == nil), "申请退款与确认收货只能有一个成功：refund=%v complete=%v", first, second)

		var status Status
		require.NoError(t, db.Raw(`SELECT status FROM orders WHERE id = ?`, created.OrderID).Scan(&status).Error)
		if first == nil {
			require.Equal(t, StatusRefundRequested, status)
			require.ErrorIs(t, second, ErrOrderStateConflict)
		} else {
			require.Equal(t, StatusCompleted, status)
			require.ErrorIs(t, first, ErrRefundNotAllowed, "已完成订单不接受退款申请")
		}
	})

	t.Run("concurrent refund requests create one document", func(t *testing.T) {
		buyers, _, skuID := seedRefundFixture(t, db, "refund-replay", 1, 2)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-replay", 1)

		ids := make(chan int64, 50)
		runConcurrent(t, 50, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			view, err := svc.RequestRefund(ctx, RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "重复点击"})
			if err != nil {
				return err
			}
			ids <- view.RefundID
			return nil
		})
		close(ids)
		var first int64
		for id := range ids {
			if first == 0 {
				first = id
				continue
			}
			require.Equal(t, first, id, "并发重复申请必须返回同一笔退款单")
		}

		var refundRows, requestedRows int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM refunds WHERE order_id = ?`, created.OrderID).Scan(&refundRows).Error)
		require.NoError(t, db.Raw(`SELECT count(*) FROM refunds WHERE order_id = ? AND status = 'REQUESTED'`, created.OrderID).Scan(&requestedRows).Error)
		require.Equal(t, int64(1), refundRows)
		require.Equal(t, int64(1), requestedRows)
	})

	t.Run("only buyer requests and only merchant decides", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "refund-authz", 2, 2)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-authz", 1)

		_, err := svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[1], OrderID: created.OrderID, Reason: "越权"})
		require.ErrorIs(t, err, ErrOrderNotFound)

		_, err = svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "本人申请"})
		require.NoError(t, err)

		// 另一个商家不能裁决这笔退款，也不能看到它。
		var otherMerchantID int64
		require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash, role) VALUES (?, ?, 'hash', 'MERCHANT') RETURNING id`,
			"other-"+uuid.NewString()+"@example.test", "other-"+uuid.NewString()[:8]).Scan(&otherMerchantID).Error)
		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: otherMerchantID, OrderID: created.OrderID, Approve: true})
		require.ErrorIs(t, err, ErrOrderNotFound)
		_, err = svc.GetRefund(context.Background(), otherMerchantID, created.OrderID, true)
		require.ErrorIs(t, err, ErrOrderNotFound)

		var status Status
		var stock, refundRows int64
		require.NoError(t, db.Raw(`SELECT status FROM orders WHERE id = ?`, created.OrderID).Scan(&status).Error)
		require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
		require.NoError(t, db.Raw(`SELECT count(*) FROM refunds WHERE order_id = ? AND status = 'SUCCEEDED'`, created.OrderID).Scan(&refundRows).Error)
		require.Equal(t, StatusRefundRequested, status)
		require.Equal(t, int64(1), stock)
		require.Zero(t, refundRows)

		// 真正的商家仍然可以正常裁决。
		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: true})
		require.NoError(t, err)
		require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
		require.Equal(t, int64(2), stock, "拒绝越权后库存只能回补一次")
	})

	t.Run("refunded order cannot be paid or fulfilled again", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "refund-terminal", 1, 1)
		svc := refundService(NewRepository(db))
		created := mustCreatePaidOrder(t, svc, buyers[0], skuID, "refund-terminal", 1)
		_, err := svc.RequestRefund(context.Background(), RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "退款"})
		require.NoError(t, err)
		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: true})
		require.NoError(t, err)

		_, err = svc.Fulfill(context.Background(), merchantID, created.OrderID)
		require.ErrorIs(t, err, ErrOrderStateConflict)
		_, err = svc.Pay(context.Background(), PayParams{
			UserID: buyers[0], OrderID: created.OrderID,
			PaymentNo: "late-pay-" + uuid.NewString(), ProviderTradeNo: "late-trade-" + uuid.NewString(), Amount: created.TotalAmount,
		})
		require.ErrorIs(t, err, ErrOrderStateConflict)
		_, err = svc.DecideRefund(context.Background(), DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: false})
		require.ErrorIs(t, err, ErrRefundAlreadyDecided, "已成功的退款不能再被拒绝")
	})
}

type refundExpectation struct {
	initialStock     int64
	orderStatus      Status
	stock            int64
	restoredLedgers  int64
	refundRows       int64
	successfulRefund int64
}

// assertRefundFacts 同时校验四个数据源：订单终态、库存余额、退款单、库存流水。
// 只校验其中一个都可能放过"状态对了但货不对"的实现，因此这里还要断言
// 库存守恒：期初库存 + 下单扣减 + 退款回补 = 当前库存。
func assertRefundFacts(t *testing.T, db *gorm.DB, orderID, skuID int64, want refundExpectation) {
	t.Helper()
	var status Status
	var stock, restoredLedgers, restoredSum, refundRows, succeeded, createdLedgers, ledgerSum int64
	require.NoError(t, db.Raw(`SELECT status FROM orders WHERE id = ?`, orderID).Scan(&status).Error)
	require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
	require.NoError(t, db.Raw(`SELECT count(*), COALESCE(sum(change), 0) FROM inventory_transactions
		WHERE order_id = ? AND reason = 'ORDER_REFUNDED'`, orderID).Row().Scan(&restoredLedgers, &restoredSum))
	require.NoError(t, db.Raw(`SELECT count(*) FROM refunds WHERE order_id = ?`, orderID).Scan(&refundRows).Error)
	require.NoError(t, db.Raw(`SELECT count(*) FROM refunds WHERE order_id = ? AND status = 'SUCCEEDED'`, orderID).Scan(&succeeded).Error)
	require.NoError(t, db.Raw(`SELECT count(*), COALESCE(sum(change), 0) FROM inventory_transactions
		WHERE order_id = ? AND reason = 'ORDER_CREATED'`, orderID).Row().Scan(&createdLedgers, &ledgerSum))

	require.Equal(t, want.orderStatus, status)
	require.Equal(t, want.stock, stock)
	require.Equal(t, want.restoredLedgers, restoredLedgers)
	require.Equal(t, want.refundRows, refundRows)
	require.Equal(t, want.successfulRefund, succeeded)
	require.Equal(t, int64(1), createdLedgers, "下单流水必须恰好一条")
	require.Equal(t, want.initialStock+ledgerSum+restoredSum, stock, "库存守恒：期初 + 流水变更 = 当前库存")
	if want.restoredLedgers == 1 {
		require.Positive(t, restoredSum, "回补流水必须是正向")
	}
}

func refundService(repo *GormRepository) *Service {
	svc := NewService(repo)
	svc.SetRefundRepository(repo)
	return svc
}

func mustCreatePaidOrder(t *testing.T, svc *Service, buyerID, skuID int64, key string, quantity int32) *CreateResult {
	t.Helper()
	created, err := svc.Create(context.Background(), CreateParams{
		UserID: buyerID, SKUID: skuID, Quantity: quantity, IdempotencyKey: key,
	})
	require.NoError(t, err)
	_, err = svc.Pay(context.Background(), PayParams{
		UserID: buyerID, OrderID: created.OrderID,
		PaymentNo: "pay-" + key, ProviderTradeNo: "trade-" + key, Amount: created.TotalAmount,
	})
	require.NoError(t, err)
	return created
}

// seedRefundFixture 与 seedOrderFixture 同构，但额外返回商家 ID（退款裁决需要）。
func seedRefundFixture(t *testing.T, db *gorm.DB, label string, buyers, stock int) ([]int64, int64, int64) {
	t.Helper()
	prefix := label + "-" + uuid.NewString()
	var merchantID int64
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash, role)
		VALUES (?, ?, 'hash', 'MERCHANT') RETURNING id`, prefix+"-merchant@example.test", prefix+"-merchant").Scan(&merchantID).Error)

	type idRow struct{ ID int64 }
	var rows []idRow
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash)
		SELECT ? || g || '@example.test', ? || g, 'hash'
		FROM generate_series(1, ?) AS g RETURNING id`, prefix+"-buyer-", prefix+"-buyer-", buyers).Scan(&rows).Error)
	require.Len(t, rows, buyers)
	buyerIDs := make([]int64, 0, buyers)
	for _, row := range rows {
		buyerIDs = append(buyerIDs, row.ID)
	}

	var productID int64
	require.NoError(t, db.Raw(`INSERT INTO products (merchant_id, title, status)
		VALUES (?, ?, 'ON_SALE') RETURNING id`, merchantID, prefix+" product").Scan(&productID).Error)
	var skuID int64
	require.NoError(t, db.Raw(`INSERT INTO skus (product_id, spec, price, status)
		VALUES (?, 'default', 1999, 'ACTIVE') RETURNING id`, productID).Scan(&skuID).Error)
	require.NoError(t, db.Exec(`INSERT INTO inventory (sku_id, quantity, version) VALUES (?, ?, 0)`, skuID, stock).Error)
	return buyerIDs, merchantID, skuID
}
