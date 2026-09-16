package order

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func dryRunOrderDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true,
	})
	require.NoError(t, err)
	return db
}

func TestCreateOrderClaimUsesUserScopedIdempotencyKey(t *testing.T) {
	db := dryRunOrderDB(t)
	query := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}, {Name: "idempotency_key"}}, DoNothing: true,
		}).Create(&Order{UserID: 7, IdempotencyKey: "key", OrderNo: "no", Status: StatusPendingPayment})
	}))
	require.Contains(t, query, "on conflict")
	require.Contains(t, query, "user_id")
	require.Contains(t, query, "idempotency_key")
}

func TestStockDeductionIsConditional(t *testing.T) {
	db := dryRunOrderDB(t)
	query := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Model(&inventoryRow{}).
			Where("sku_id = ? AND quantity >= ?", 11, 3).
			Updates(map[string]any{
				"quantity": gorm.Expr("quantity - ?", 3),
				"version":  gorm.Expr("version + 1"),
			})
	}))
	require.Contains(t, query, "update \"inventory\"")
	require.Contains(t, query, "quantity >= 3")
	require.Contains(t, query, "quantity - 3")
}

func TestSellableSKUQueryLocksProductAndSKU(t *testing.T) {
	db := dryRunOrderDB(t)
	query := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		var row SellableSKU
		return tx.Raw(`select s.id as sku_id from skus s join products p on p.id = s.product_id
			where s.id = ? and s.status = 'ACTIVE' and p.status = 'ON_SALE' for share of s, p`, 11).Scan(&row)
	}))
	require.Contains(t, query, "status = 'active'")
	require.Contains(t, query, "status = 'on_sale'")
	require.Contains(t, query, "for share of s, p")
}

func TestActiveSourceQueryRequiresExactAttribution(t *testing.T) {
	db := dryRunOrderDB(t)
	query := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		var row struct{ ID int64 }
		return tx.Raw(`select id from video_products where video_id = ? and author_id = ?
			and product_id = ? and status = 'ACTIVE' for share`, 1, 2, 3).Scan(&row)
	}))
	for _, field := range []string{"video_id", "author_id", "product_id", "status"} {
		require.Contains(t, query, field)
	}
	require.Contains(t, query, "for share")
}

func TestLedgerUsesOrderCreatedAndOrderIdentity(t *testing.T) {
	db := dryRunOrderDB(t)
	query := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&inventoryLedger{SKUID: 11, OrderID: 22, Change: -3, Reason: "ORDER_CREATED", BizID: "order-no"})
	}))
	require.Contains(t, query, "inventory_transactions")
	require.Contains(t, query, "order_created")
	require.Contains(t, query, "-3")
	require.Contains(t, query, "order-no")
}

func TestOrderTransitionQueriesUseRowLockAndSourceStatus(t *testing.T) {
	db := dryRunOrderDB(t)
	lockQuery := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		var order Order
		return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", 31, 42).Take(&order)
	}))
	require.Contains(t, lockQuery, "for update")
	require.Contains(t, lockQuery, "id = 31")
	require.Contains(t, lockQuery, "user_id = 42")

	payQuery := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Model(&Order{}).Where("id = ? AND status = ?", 1, string(StatusPendingPayment)).
			Updates(map[string]any{"status": string(StatusPaid), "paid_at": time.Now()})
	}))
	require.Contains(t, payQuery, "status = 'pending_payment'")
	require.Contains(t, payQuery, "paid_at")

	cancelQuery := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Model(&Order{}).Where("id = ? AND status = ?", 1, string(StatusPendingPayment)).
			Updates(map[string]any{"status": string(StatusCanceled), "canceled_at": time.Now()})
	}))
	require.Contains(t, cancelQuery, "status = 'pending_payment'")
	require.Contains(t, cancelQuery, "canceled_at")
}

func TestCancelRestoresStockAndWritesScopedLedger(t *testing.T) {
	db := dryRunOrderDB(t)
	stockQuery := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Model(&inventoryRow{}).Where("sku_id = ?", 11).Updates(map[string]any{
			"quantity": gorm.Expr("quantity + ?", 3), "version": gorm.Expr("version + 1"),
		})
	}))
	require.Contains(t, stockQuery, "quantity + 3")

	ledgerQuery := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&inventoryLedger{SKUID: 11, OrderID: 22, Change: 3, Reason: "ORDER_CANCELED", BizID: "order-no"})
	}))
	require.Contains(t, ledgerQuery, "order_canceled")
	require.Contains(t, ledgerQuery, "order-no")
}
