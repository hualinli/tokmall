package inventory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type inventorySQLLogger struct {
	logger.Interface
	queries []string
}

func (l *inventorySQLLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	l.queries = append(l.queries, sql)
}

func dryRunInventoryRepo(t *testing.T, log *inventorySQLLogger) *GormRepository {
	t.Helper()
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 log,
	})
	require.NoError(t, err)
	return NewRepository(db)
}

func TestGormRepositoryAdjustClaimsScopedIdempotencyKey(t *testing.T) {
	log := &inventorySQLLogger{Interface: logger.Discard}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
		Logger:               log,
	})
	require.NoError(t, err)
	tx := &InventoryTransaction{SkuID: 11, Change: 5, Reason: ReasonAdjust, BizID: "client-key"}
	claimSQL := db.ToSQL(func(txDB *gorm.DB) *gorm.DB {
		return txDB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "sku_id"}, {Name: "reason"}, {Name: "biz_id"}},
			DoNothing: true,
		}).Create(tx)
	})
	updateSQL := db.ToSQL(func(txDB *gorm.DB) *gorm.DB {
		return txDB.Model(&Inventory{}).
			Where("sku_id = ? AND quantity + ? >= 0", tx.SkuID, tx.Change).
			Updates(map[string]any{
				"quantity":   gorm.Expr("quantity + ?", tx.Change),
				"version":    gorm.Expr("version + 1"),
				"updated_at": time.Now(),
			})
	})
	require.Contains(t, strings.ToLower(claimSQL), "on conflict")
	require.Contains(t, strings.ToLower(claimSQL), "sku_id")
	require.Contains(t, strings.ToLower(claimSQL), "reason")
	require.Contains(t, strings.ToLower(claimSQL), "biz_id")
	require.Contains(t, strings.ToLower(updateSQL), "quantity +")
	require.Contains(t, strings.ToLower(updateSQL), ">= 0")
}

func TestGormRepositoryFindSkuOwnerUsesSharedReadModel(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
	})
	require.NoError(t, err)
	query := strings.ToLower(db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		var row struct {
			ProductID  int64 `gorm:"column:product_id"`
			MerchantID int64 `gorm:"column:merchant_id"`
		}
		return tx.Table("skus s").
			Select("s.product_id AS product_id", "p.merchant_id AS merchant_id").
			Joins("JOIN products p ON p.id = s.product_id").
			Where("s.id = ?", 11).
			Take(&row)
	}))
	require.Contains(t, query, "from skus s")
	require.Contains(t, query, "join products")
	require.Contains(t, query, "merchant_id")
}
