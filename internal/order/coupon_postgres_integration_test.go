package order

import (
	"context"
	"errors"
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

func TestPostgresClosedCouponMigrationRepairsHistory(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("PostgreSQL integration is disabled")
	}
	db := newIsolatedPostgres(t, strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN")))
	down, err := os.ReadFile("../../migrations/000024_closed_order_coupon.down.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(down)).Error)
	buyers, merchantID, skuID := seedRefundFixture(t, db, "repair", 1, 5)
	uc := seedUserCoupon(t, db, buyers[0], merchantID, 1000, 500)
	svc := refundService(NewRepository(db))
	ctx := context.Background()
	created, err := svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: skuID, Quantity: 1, IdempotencyKey: "repair-old", UserCouponID: &uc})
	require.NoError(t, err)
	// Simulate the old cancellation implementation, which did not release coupons.
	require.NoError(t, db.Exec("UPDATE orders SET status='CANCELED',canceled_at=now() WHERE id=?", created.OrderID).Error)
	up, err := os.ReadFile("../../migrations/000024_closed_order_coupon.up.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(up)).Error)
	var status string
	require.NoError(t, db.Raw("SELECT status FROM user_coupons WHERE id=?", uc).Scan(&status).Error)
	require.Equal(t, "UNUSED", status)
	_, err = svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: skuID, Quantity: 1, IdempotencyKey: "repair-new", UserCouponID: &uc})
	require.NoError(t, err)
	require.Error(t, db.Exec(string(down)).Error, "rollback must preserve reused coupon history")
}

// 优惠券与订单的集成证明：金额拆分、一券一单、幂等重放、退款退券。
// 这些都是"券"这个跨模块特性最容易出错的地方（DomTok 在这里的字段语义全乱了）。
func TestPostgresOrderCouponIntegration(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	db := newIsolatedPostgres(t, dsn)
	repo := NewRepository(db)
	svc := refundService(repo)
	ctx := context.Background()
	for _, mode := range []string{"cancel", "expire", "lazy-expire", "expired-coupon"} {
		t.Run("closed order releases coupon "+mode, func(t *testing.T) {
			buyers, merchantID, skuID := seedRefundFixture(t, db, mode[:3], 1, 5)
			uc := seedUserCoupon(t, db, buyers[0], merchantID, 1000, 500)
			created, err := svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: skuID, Quantity: 1, IdempotencyKey: "close-" + mode, UserCouponID: &uc})
			require.NoError(t, err)
			if mode == "expired-coupon" {
				require.NoError(t, db.Exec("UPDATE coupons SET valid_to=now()-interval '1 minute' WHERE id=(SELECT coupon_id FROM user_coupons WHERE id=?)", uc).Error)
			}
			switch mode {
			case "expire", "lazy-expire":
				require.NoError(t, db.Exec("UPDATE orders SET created_at=now()-interval '2 minutes', expires_at=now()-interval '1 minute' WHERE id=?", created.OrderID).Error)
				if mode == "expire" {
					expired, err := repo.ExpireNext(ctx, time.Now())
					require.NoError(t, err)
					require.True(t, expired)
				} else {
					_, err := svc.Pay(ctx, PayParams{UserID: buyers[0], OrderID: created.OrderID, PaymentNo: "close-pay", ProviderTradeNo: "close-trade", Amount: created.TotalAmount})
					require.ErrorIs(t, err, ErrOrderExpired)
				}
			default:
				_, err := svc.Cancel(ctx, CancelParams{UserID: buyers[0], OrderID: created.OrderID})
				require.NoError(t, err)
			}
			var status string
			require.NoError(t, db.Raw("SELECT status FROM user_coupons WHERE id=?", uc).Scan(&status).Error)
			if mode == "expired-coupon" {
				require.Equal(t, "EXPIRED", status)
				return
			}
			require.Equal(t, "UNUSED", status)
			reused, err := svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: skuID, Quantity: 1, IdempotencyKey: "reuse-" + mode, UserCouponID: &uc})
			require.NoError(t, err)
			require.NotEqual(t, created.OrderID, reused.OrderID)
			var historical int64
			require.NoError(t, db.Raw("SELECT user_coupon_id FROM orders WHERE id=?", created.OrderID).Scan(&historical).Error)
			require.Equal(t, uc, historical)
		})
	}

	t.Run("order splits amount and marks the coupon used", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "coupon-order", 1, 5)
		// 商品单价 1999 分：门槛 1000 分满足，券面额 500 分。
		userCouponID := seedUserCoupon(t, db, buyers[0], merchantID, 1000, 500)

		created, err := svc.Create(ctx, CreateParams{
			UserID: buyers[0], SKUID: skuID, Quantity: 1,
			IdempotencyKey: "coupon-order", UserCouponID: &userCouponID,
		})
		require.NoError(t, err)
		require.Equal(t, Amount(1999), created.OriginalAmount)
		require.Equal(t, Amount(500), created.DiscountAmount)
		require.Equal(t, Amount(1499), created.TotalAmount, "实付 = 原价 - 优惠")

		var row struct {
			Status  string
			OrderID *int64
			UsedAt  *time.Time
		}
		require.NoError(t, db.Raw(`SELECT status, order_id, used_at FROM user_coupons WHERE id = ?`, userCouponID).
			Row().Scan(&row.Status, &row.OrderID, &row.UsedAt))
		require.Equal(t, "USED", row.Status)
		require.NotNil(t, row.OrderID)
		require.Equal(t, created.OrderID, *row.OrderID, "券必须指向用它下单的那张订单")
		require.NotNil(t, row.UsedAt)

		// 支付金额等于实付金额
		_, err = svc.Pay(ctx, PayParams{
			UserID: buyers[0], OrderID: created.OrderID,
			PaymentNo: "coupon-pay", ProviderTradeNo: "coupon-trade", Amount: created.TotalAmount,
		})
		require.NoError(t, err)

		// 重复下单（同幂等键同载荷）→ 返回原订单，不会二次核销券
		replayed, err := svc.Create(ctx, CreateParams{
			UserID: buyers[0], SKUID: skuID, Quantity: 1,
			IdempotencyKey: "coupon-order", UserCouponID: &userCouponID,
		})
		require.NoError(t, err)
		require.True(t, replayed.Replayed)
		require.Equal(t, created.OrderID, replayed.OrderID)

		var coupons int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM user_coupons WHERE coupon_id = (
			SELECT coupon_id FROM user_coupons WHERE id = ?)`, userCouponID).Scan(&coupons).Error)
		require.Equal(t, int64(1), coupons, "重放不得产生第二张持券")
	})

	t.Run("refund returns the coupon when still valid", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "coupon-refund", 1, 3)
		userCouponID := seedUserCoupon(t, db, buyers[0], merchantID, 1000, 500)

		created, err := svc.Create(ctx, CreateParams{
			UserID: buyers[0], SKUID: skuID, Quantity: 1,
			IdempotencyKey: "coupon-refund", UserCouponID: &userCouponID,
		})
		require.NoError(t, err)
		_, err = svc.Pay(ctx, PayParams{
			UserID: buyers[0], OrderID: created.OrderID,
			PaymentNo: "coupon-refund-pay", ProviderTradeNo: "coupon-refund-trade", Amount: created.TotalAmount,
		})
		require.NoError(t, err)

		_, err = svc.RequestRefund(ctx, RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "不想要了"})
		require.NoError(t, err)
		_, err = svc.DecideRefund(ctx, DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: true})
		require.NoError(t, err)

		var status string
		var orderID *int64
		require.NoError(t, db.Raw(`SELECT status, order_id FROM user_coupons WHERE id = ?`, userCouponID).
			Row().Scan(&status, &orderID))
		require.Equal(t, "UNUSED", status, "退款成功后券必须退回可再用状态")
		require.Nil(t, orderID, "退券要清空订单引用，否则这张券再也用不出去")
		reused, err := svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: skuID, Quantity: 1, IdempotencyKey: "coupon-reuse", UserCouponID: &userCouponID})
		require.NoError(t, err, "returned coupon must be usable in a new order")
		require.NotEqual(t, created.OrderID, reused.OrderID)
		var originalCouponID int64
		require.NoError(t, db.Raw("SELECT user_coupon_id FROM orders WHERE id = ?", created.OrderID).Scan(&originalCouponID).Error)
		require.Equal(t, userCouponID, originalCouponID, "refund preserves the historical coupon reference")

	})

	t.Run("refund marks the coupon expired when it lapsed", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "coupon-expired", 1, 3)
		userCouponID := seedUserCoupon(t, db, buyers[0], merchantID, 1000, 500)

		created, err := svc.Create(ctx, CreateParams{
			UserID: buyers[0], SKUID: skuID, Quantity: 1,
			IdempotencyKey: "coupon-expired", UserCouponID: &userCouponID,
		})
		require.NoError(t, err)
		_, err = svc.Pay(ctx, PayParams{
			UserID: buyers[0], OrderID: created.OrderID,
			PaymentNo: "coupon-expired-pay", ProviderTradeNo: "coupon-expired-trade", Amount: created.TotalAmount,
		})
		require.NoError(t, err)
		// 券在退款之前过期
		require.NoError(t, db.Exec(`UPDATE coupons SET valid_to = now() - interval '1 minute'
			WHERE id = (SELECT coupon_id FROM user_coupons WHERE id = ?)`, userCouponID).Error)

		_, err = svc.RequestRefund(ctx, RequestRefundParams{UserID: buyers[0], OrderID: created.OrderID, Reason: "退款"})
		require.NoError(t, err)
		_, err = svc.DecideRefund(ctx, DecideRefundParams{MerchantID: merchantID, OrderID: created.OrderID, Approve: true})
		require.NoError(t, err)

		var status string
		require.NoError(t, db.Raw(`SELECT status FROM user_coupons WHERE id = ?`, userCouponID).Scan(&status).Error)
		require.Equal(t, "EXPIRED", status, "已过期的券退回也没用，状态要如实反映")
	})

	t.Run("rejects coupons that are not usable", func(t *testing.T) {
		buyers, merchantID, skuID := seedRefundFixture(t, db, "coupon-reject", 2, 10)

		t.Run("threshold not met", func(t *testing.T) {
			// 门槛 5000 分 > 商品单价 1999 分
			userCouponID := seedUserCoupon(t, db, buyers[0], merchantID, 5000, 1000)
			_, err := svc.Create(ctx, CreateParams{
				UserID: buyers[0], SKUID: skuID, Quantity: 1,
				IdempotencyKey: "coupon-threshold", UserCouponID: &userCouponID,
			})
			require.ErrorIs(t, err, ErrCouponThresholdNotMet)
		})

		t.Run("coupon belongs to another merchant", func(t *testing.T) {
			_, otherMerchant, _ := seedRefundFixture(t, db, "coupon-other", 1, 5)
			uc := seedUserCoupon(t, db, buyers[0], otherMerchant, 1000, 500)
			_, err := svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: skuID, Quantity: 1, IdempotencyKey: "coupon-merchant", UserCouponID: &uc})
			require.ErrorIs(t, err, ErrCouponNotUsable)
			var status string
			require.NoError(t, db.Raw("SELECT status FROM user_coupons WHERE id = ?", uc).Scan(&status).Error)
			require.Equal(t, "UNUSED", status)
		})

		t.Run("coupon belongs to another user", func(t *testing.T) {
			userCouponID := seedUserCoupon(t, db, buyers[1], merchantID, 1000, 500)
			_, err := svc.Create(ctx, CreateParams{
				UserID: buyers[0], SKUID: skuID, Quantity: 1,
				IdempotencyKey: "coupon-owner", UserCouponID: &userCouponID,
			})
			require.ErrorIs(t, err, ErrCouponNotFound, "别人的券与不存在的券返回同一错误，不泄露存在性")
		})

		t.Run("coupon already used", func(t *testing.T) {
			userCouponID := seedUserCoupon(t, db, buyers[0], merchantID, 1000, 500)
			_, err := svc.Create(ctx, CreateParams{
				UserID: buyers[0], SKUID: skuID, Quantity: 1,
				IdempotencyKey: "coupon-used-1", UserCouponID: &userCouponID,
			})
			require.NoError(t, err)
			_, err = svc.Create(ctx, CreateParams{
				UserID: buyers[0], SKUID: skuID, Quantity: 1,
				IdempotencyKey: "coupon-used-2", UserCouponID: &userCouponID,
			})
			require.ErrorIs(t, err, ErrCouponConflict, "一张券只能用在一张订单上")
		})
	})
}

// seedUserCoupon 造一张已领取的券：门槛 threshold、面额 discount，有效期 1 小时。
func seedUserCoupon(t *testing.T, db *gorm.DB, userID, merchantID, threshold, discount int64) int64 {
	t.Helper()
	prefix := "uc-" + uuid.NewString()[:8]
	var couponID int64
	require.NoError(t, db.Raw(`INSERT INTO coupons
		(merchant_id, title, threshold_amount, discount_amount, total_quantity, issued_quantity, per_user_limit, status, valid_from, valid_to)
		VALUES (?, ?, ?, ?, 100, 1, 1, 'ACTIVE', now() - interval '1 minute', now() + interval '1 hour')
		RETURNING id`, merchantID, prefix, threshold, discount).Scan(&couponID).Error)
	var userCouponID int64
	require.NoError(t, db.Raw(`INSERT INTO user_coupons (coupon_id, user_id, status)
		VALUES (?, ?, 'UNUSED') RETURNING id`, couponID, userID).Scan(&userCouponID).Error)
	return userCouponID
}

// 同键重试必须返回原订单；不同载荷冲突不能被已用券错误遮住。
func TestPostgresConcurrentCouponIdempotency(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("PostgreSQL integration is disabled")
	}
	db := newIsolatedPostgres(t, strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN")))
	for _, mode := range []string{"replay", "payload-conflict", "different-keys"} {
		t.Run(mode, func(t *testing.T) {
			buyers, merchant, sku := seedRefundFixture(t, db, "race-"+mode[:3], 1, 100)
			uc := seedUserCoupon(t, db, buyers[0], merchant, 1000, 500)
			svc := refundService(NewRepository(db))
			const workers = 20
			results := make(chan *CreateResult, workers)
			var sequence, conflicts atomic.Int64
			runConcurrent(t, workers, func() error {
				i := sequence.Add(1)
				key, quantity := "coupon-race-"+mode+"-same", int32(1)
				if mode == "payload-conflict" {
					quantity = 1 + int32(i%2)
				}
				if mode == "different-keys" {
					key = fmt.Sprintf("coupon-race-%s-%d", mode, i)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				result, err := svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: sku, Quantity: quantity, IdempotencyKey: key, UserCouponID: &uc})
				if mode == "payload-conflict" && errors.Is(err, ErrIdempotencyConflict) || mode == "different-keys" && errors.Is(err, ErrCouponConflict) {
					conflicts.Add(1)
					return nil
				}
				if err == nil {
					results <- result
				}
				return err
			})
			close(results)
			var first *CreateResult
			var fresh, successes int64
			for result := range results {
				require.NotNil(t, result)
				if first == nil {
					first = result
				}
				require.Equal(t, first.OrderID, result.OrderID)
				require.Equal(t, first.TotalAmount, result.TotalAmount)
				if !result.Replayed {
					fresh++
				}
				successes++
			}
			require.NotNil(t, first)
			require.Equal(t, int64(1), fresh)
			switch mode {
			case "replay":
				require.Equal(t, int64(workers), successes)
				require.Zero(t, conflicts.Load())
			case "payload-conflict":
				require.Equal(t, int64(workers/2), successes)
				require.Equal(t, int64(workers/2), conflicts.Load())
			case "different-keys":
				require.Equal(t, int64(1), successes)
				require.Equal(t, int64(workers-1), conflicts.Load())
			}
			var quantity int64
			require.NoError(t, db.Raw("SELECT quantity FROM order_items WHERE order_id = ?", first.OrderID).Scan(&quantity).Error)
			assertOrderDatabaseFacts(t, db, sku, "coupon-race-"+mode+"-%", 100-quantity, 1, -quantity)
			var status string
			var couponOrder int64
			require.NoError(t, db.Raw("SELECT status, order_id FROM user_coupons WHERE id = ?", uc).Row().Scan(&status, &couponOrder))
			require.Equal(t, "USED", status)
			require.Equal(t, first.OrderID, couponOrder)
		})
	}
}
