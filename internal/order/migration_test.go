package order

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

func TestOrderFoundationMigrationDefinesCoreInvariants(t *testing.T) {
	up := readMigration(t, "000012_create_order_foundation.up.sql")
	down := readMigration(t, "000012_create_order_foundation.down.sql")

	for _, fragment := range []string{
		"create table video_products",
		"unique (video_id, product_id)",
		"create table orders",
		"unique (user_id, idempotency_key)",
		"request_fingerprint",
		"orders_source_all_or_none_check",
		"orders_state_timestamps_check",
		"orders_expiry_after_creation_check",
		"where status = 'pending_payment'",
		"create table order_items",
		"merchant_id_snapshot",
		"product_title_snapshot",
		"sku_spec_snapshot",
		"unit_price",
		"line_amount",
		"unique (order_id)",
		"create table payments",
		"idx_payments_order_created",
		"payments_provider_trade_no_nonempty_check",
		"where provider_trade_no is not null",
	} {
		require.Contains(t, up, fragment)
	}

	// 多次支付尝试是显式能力，payments 段不能把 order_id 设为唯一。
	paymentSection := strings.Split(up, "create table payments")[1]
	require.NotContains(t, paymentSection, "unique (order_id)")
	for _, table := range []string{"payments", "order_items", "orders", "video_products"} {
		require.Contains(t, down, "drop table if exists "+table)
	}
}

func TestOrderAttributionUsesSnapshotsWithoutForeignKeys(t *testing.T) {
	up := readMigration(t, "000012_create_order_foundation.up.sql")
	require.NotContains(t, up, "source_video_id     bigint references")
	require.NotContains(t, up, "source_author_id    bigint references")
	require.NotContains(t, up, "source_product_id   bigint references")
}
