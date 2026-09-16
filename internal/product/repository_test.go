package product

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestGormRepositoryCreateSKUInitializesInventoryInSameTransaction(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
	})
	require.NoError(t, err)
	skuSQL := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&SKU{ProductID: 101, Spec: "black/L", Price: 9900, Status: SKUStatusActive})
	})
	inventorySQL := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Exec("INSERT INTO inventory (sku_id, quantity, version) VALUES (?, 0, 0)", 11)
	})
	require.Contains(t, strings.ToLower(skuSQL), "insert into \"skus\"")
	require.Contains(t, strings.ToLower(inventorySQL), "insert into inventory")
	require.Contains(t, strings.ToLower(inventorySQL), "quantity")
	require.Contains(t, strings.ToLower(inventorySQL), "version")
}
