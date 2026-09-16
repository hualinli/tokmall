package inventory

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func readMigration(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	b, err := os.ReadFile(filepath.Join(root, "migrations", name))
	require.NoError(t, err)
	return strings.ToLower(string(b))
}

func TestInventoryInvariantMigrationIsForwardOnlyAndReversible(t *testing.T) {
	up := readMigration(t, "000011_inventory_idempotency_and_invariants.up.sql")
	down := readMigration(t, "000011_inventory_idempotency_and_invariants.down.sql")

	for _, fragment := range []string{
		"insert into inventory",
		"unique (sku_id, reason, biz_id)",
		"check (merchant_id > 0)",
		"check (btrim(title) <> '')",
		"check (status in ('on_sale', 'off_sale', 'deleted'))",
		"check (price > 0)",
		"check (status in ('active', 'disabled'))",
		"check (quantity >= 0)",
		"check (change <> 0)",
		"check (reason in ('adjust', 'order_created', 'order_canceled', 'order_expired'))",
	} {
		require.Contains(t, up, fragment)
	}
	for _, fragment := range []string{
		"drop constraint if exists inventory_transactions_reason_check",
		"drop constraint if exists inventory_transactions_sku_reason_biz_id_key",
		"unique (biz_id)",
	} {
		require.Contains(t, down, fragment)
	}
	require.NotContains(t, up, "000007")
	require.NotContains(t, up, "000008")
	require.NotContains(t, up, "000009")
	require.NotContains(t, up, "000010")
}
