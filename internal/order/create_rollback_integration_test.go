package order

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/testsupport"
)

// 库存更新放到最后后，库存不足仍须撤销全部提前准备的事实。
func TestPostgresInsufficientStockRollsBackPreparedFactsAndCoupon(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	buyers, merchant, sku := seedRefundFixture(t, db, "prepared-rollback", 1, 0)
	couponID := seedUserCoupon(t, db, buyers[0], merchant, 1000, 500)
	svc := NewService(NewRepository(db))
	params := CreateParams{UserID: buyers[0], SKUID: sku, Quantity: 1,
		IdempotencyKey: "retry-after-restock", UserCouponID: &couponID}
	_, err := svc.Create(context.Background(), params)
	require.ErrorIs(t, err, ErrInsufficientStock)
	for _, table := range []string{"orders", "order_items", "inventory_transactions", "outbox_events"} {
		var count int64
		require.NoError(t, db.Table(table).Count(&count).Error)
		require.Zero(t, count, "库存不足不得遗留 %s", table)
	}
	var status string
	require.NoError(t, db.Raw("SELECT status FROM user_coupons WHERE id=?", couponID).Scan(&status).Error)
	require.Equal(t, "UNUSED", status)
	var stock struct{ Quantity, Version int64 }
	require.NoError(t, db.Raw("SELECT quantity, version FROM inventory WHERE sku_id=?", sku).Scan(&stock).Error)
	require.Zero(t, stock.Quantity)
	require.Zero(t, stock.Version)

	// 失败不占用幂等键；补货后同请求可以成功，随后重放也不重复扣减。
	require.NoError(t, db.Exec("UPDATE inventory SET quantity=1 WHERE sku_id=?", sku).Error)
	created, err := svc.Create(context.Background(), params)
	require.NoError(t, err)
	replayed, err := svc.Create(context.Background(), params)
	require.NoError(t, err)
	require.Equal(t, created.OrderID, replayed.OrderID)
	assertOrderDatabaseFacts(t, db, sku, params.IdempotencyKey, 0, 1, -1)
	var events int64
	require.NoError(t, db.Table("outbox_events").Count(&events).Error)
	require.Equal(t, int64(1), events)
	require.NoError(t, db.Raw("SELECT status FROM user_coupons WHERE id=?", couponID).Scan(&status).Error)
	require.Equal(t, "USED", status)
}
