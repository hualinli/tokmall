package coupon_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lihualin/tokmall/internal/coupon"
	"github.com/lihualin/tokmall/internal/testsupport"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 领券并发证明：券的发行量是**券级别**的共享资源，必须在真实数据库上验证
// "不超发、不超领"。内存 fake 证明不了行锁与条件更新的行为。
//
//	TOKMALL_RUN_PG_INTEGRATION=1 \
//	TOKMALL_TEST_DATABASE_DSN='host=127.0.0.1 user=tokmall password=tokmall dbname=tokmall port=5432 sslmode=disable' \
//	go test ./internal/coupon -count=1 -v
func TestPostgresCouponClaimConcurrency(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	repo := coupon.NewRepository(db)
	svc := coupon.NewService(repo)
	ctx := context.Background()

	t.Run("100 concurrent claims on 10 coupons issue exactly 10", func(t *testing.T) {
		merchantID, userIDs := seedCouponFixture(t, db, 100)
		view, err := svc.CreateCoupon(ctx, coupon.CreateCouponParams{
			MerchantID: merchantID, Title: "限量 10 张", ThresholdAmount: 10000, DiscountAmount: 2000,
			TotalQuantity: 10, PerUserLimit: 1,
			ValidFrom: time.Now().Add(-time.Minute), ValidTo: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)

		var success, soldOut, unexpected atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, userID := range userIDs {
			wg.Add(1)
			go func(userID int64) {
				defer wg.Done()
				<-start
				callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				_, err := svc.Claim(callCtx, userID, view.CouponID)
				switch {
				case err == nil:
					success.Add(1)
				case errors.Is(err, coupon.ErrSoldOut):
					soldOut.Add(1)
				default:
					unexpected.Add(1)
				}
			}(userID)
		}
		close(start)
		wg.Wait()

		require.Equal(t, int64(10), success.Load(), "恰好 10 张被领走")
		require.Equal(t, int64(90), soldOut.Load())
		require.Zero(t, unexpected.Load())

		var issued, rows int64
		require.NoError(t, db.Raw(`SELECT issued_quantity FROM coupons WHERE id = ?`, view.CouponID).Scan(&issued).Error)
		require.NoError(t, db.Raw(`SELECT count(*) FROM user_coupons WHERE coupon_id = ?`, view.CouponID).Scan(&rows).Error)
		require.Equal(t, int64(10), issued, "发行量计数器与实际持券数必须一致")
		require.Equal(t, int64(10), rows)
	})

	t.Run("per-user limit holds under concurrent retries", func(t *testing.T) {
		merchantID, userIDs := seedCouponFixture(t, db, 1)
		view, err := svc.CreateCoupon(ctx, coupon.CreateCouponParams{
			MerchantID: merchantID, Title: "每人限领 2 张", ThresholdAmount: 10000, DiscountAmount: 2000,
			TotalQuantity: 0, PerUserLimit: 2,
			ValidFrom: time.Now().Add(-time.Minute), ValidTo: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)

		// 同一用户并发领 20 次：只有 2 次成功，其余都是"已达限领"。
		var success, alreadyClaimed, unexpected atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				_, err := svc.Claim(callCtx, userIDs[0], view.CouponID)
				switch {
				case err == nil:
					success.Add(1)
				case errors.Is(err, coupon.ErrAlreadyClaimed):
					alreadyClaimed.Add(1)
				default:
					unexpected.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()

		require.Equal(t, int64(2), success.Load(), "每人限领 2 张必须精确成立")
		require.Equal(t, int64(18), alreadyClaimed.Load())
		require.Zero(t, unexpected.Load())

		var rows, issued int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM user_coupons WHERE coupon_id = ? AND user_id = ?`,
			view.CouponID, userIDs[0]).Scan(&rows).Error)
		require.NoError(t, db.Raw(`SELECT issued_quantity FROM coupons WHERE id = ?`, view.CouponID).Scan(&issued).Error)
		require.Equal(t, int64(2), rows)
		require.Equal(t, int64(2), issued, "被拒绝的领取必须回滚发行量计数")
	})

	t.Run("database rejects over-issuance even without the row lock", func(t *testing.T) {
		// 直接绕过仓储的条件更新，用一条裸 UPDATE 试图把发行量推过上限：
		// CHECK 约束必须拒绝——这是"应用层写错也不会脏数据"的最后一道防线。
		merchantID, _ := seedCouponFixture(t, db, 1)
		view, err := svc.CreateCoupon(ctx, coupon.CreateCouponParams{
			MerchantID: merchantID, Title: "总量 1", ThresholdAmount: 100, DiscountAmount: 10,
			TotalQuantity: 1, PerUserLimit: 1,
			ValidFrom: time.Now().Add(-time.Minute), ValidTo: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)

		err = db.Exec(`UPDATE coupons SET issued_quantity = issued_quantity + 5 WHERE id = ?`, view.CouponID).Error
		require.Error(t, err, "issued_quantity 超过 total_quantity 必须被数据库拒绝")
		require.Contains(t, err.Error(), "coupons_issued_not_exceed_total_check")
	})
}

// seedCouponFixture 造一个商家 + N 个买家。
func seedCouponFixture(t *testing.T, db *gorm.DB, buyers int) (int64, []int64) {
	t.Helper()
	prefix := "coupon-" + uuid.NewString()[:8]
	var merchantID int64
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash, role)
		VALUES (?, ?, 'hash', 'MERCHANT') RETURNING id`,
		prefix+"-merchant@example.test", prefix+"-merchant").Scan(&merchantID).Error)

	type idRow struct{ ID int64 }
	var rows []idRow
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash)
		SELECT ? || g || '@example.test', ? || g, 'hash'
		FROM generate_series(1, ?) AS g RETURNING id`, prefix+"-buyer-", prefix+"-buyer-", buyers).Scan(&rows).Error)
	require.Len(t, rows, buyers)
	ids := make([]int64, 0, buyers)
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return merchantID, ids
}

var _ = fmt.Sprintf
