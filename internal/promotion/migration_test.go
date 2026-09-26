package promotion

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestActiveLimitMigrationHasDatabaseConcurrencyGuard(t *testing.T) {
	body, err := os.ReadFile("../../migrations/000013_enforce_video_product_active_limit.up.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(body))
	require.Contains(t, sql, "pg_advisory_xact_lock")
	require.Contains(t, sql, "video_products_active_limit_check")
	require.Contains(t, sql, "before insert or update")
}
