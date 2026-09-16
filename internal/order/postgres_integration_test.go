package order

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const postgresIntegrationSwitch = "TOKMALL_RUN_PG_INTEGRATION"

// TestPostgresOrderConcurrencyProof 是显式开启的真实 PostgreSQL 证据测试。
// 普通 go test 不读取数据库配置，也不会连接或修改任何数据库。
func TestPostgresOrderConcurrencyProof(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn, "integration switch is on, TOKMALL_TEST_DATABASE_DSN is required")
	db := newIsolatedPostgres(t, dsn)

	t.Run("500 buyers cannot oversell 100 units", func(t *testing.T) {
		buyerIDs, skuID := seedOrderFixture(t, db, "oversell", 500, 100)
		svc := NewService(NewRepository(db))

		var success, insufficient, unexpected atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, userID := range buyerIDs {
			wg.Add(1)
			go func(i int, userID int64) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := svc.Create(ctx, CreateParams{
					UserID: userID, SKUID: skuID, Quantity: 1,
					IdempotencyKey: fmt.Sprintf("oversell-%03d", i),
				})
				switch {
				case err == nil:
					success.Add(1)
				case errors.Is(err, ErrInsufficientStock):
					insufficient.Add(1)
				default:
					unexpected.Add(1)
				}
			}(i, userID)
		}
		close(start)
		wg.Wait()

		require.Equal(t, int64(100), success.Load())
		require.Equal(t, int64(400), insufficient.Load())
		require.Zero(t, unexpected.Load())
		assertOrderDatabaseFacts(t, db, skuID, "oversell-%", 0, 100, -100)
	})

	t.Run("same user and key deduct once", func(t *testing.T) {
		buyerIDs, skuID := seedOrderFixture(t, db, "replay", 1, 100)
		svc := NewService(NewRepository(db))
		const workers = 100
		results := make(chan *CreateResult, workers)
		errs := make(chan error, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				result, err := svc.Create(ctx, CreateParams{
					UserID: buyerIDs[0], SKUID: skuID, Quantity: 1, IdempotencyKey: "same-key",
				})
				results <- result
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		var orderID int64
		for err := range errs {
			require.NoError(t, err)
		}
		for result := range results {
			require.NotNil(t, result)
			if orderID == 0 {
				orderID = result.OrderID
			}
			require.Equal(t, orderID, result.OrderID)
		}
		assertOrderDatabaseFacts(t, db, skuID, "same-key", 99, 1, -1)
	})
}

func newIsolatedPostgres(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := "tokmall_order_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)

	scopedDSN, err := dsnWithSearchPath(dsn, schema)
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(scopedDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)

	t.Cleanup(func() {
		_ = sqlDB.Close()
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		adminSQL, err := admin.DB()
		if err == nil {
			_ = adminSQL.Close()
		}
	})
	applyAllMigrations(t, db)
	return db
}

func dsnWithSearchPath(dsn, schema string) (string, error) {
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return dsn + " search_path=" + schema, nil
}

func applyAllMigrations(t *testing.T, db *gorm.DB) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	sort.Strings(paths)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoErrorf(t, db.Exec(string(body)).Error, "apply %s", filepath.Base(path))
	}
}

func seedOrderFixture(t *testing.T, db *gorm.DB, label string, buyers, quantity int) ([]int64, int64) {
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
	require.NoError(t, db.Exec(`INSERT INTO inventory (sku_id, quantity, version) VALUES (?, ?, 0)`, skuID, quantity).Error)
	return buyerIDs, skuID
}

func assertOrderDatabaseFacts(t *testing.T, db *gorm.DB, skuID int64, keyPattern string, wantStock, wantOrders, wantLedger int64) {
	t.Helper()
	var stock int64
	require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
	require.Equal(t, wantStock, stock)

	var orders, items, halfOrders, ledgerCount, ledgerSum int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM orders WHERE idempotency_key LIKE ?`, keyPattern).Scan(&orders).Error)
	require.NoError(t, db.Raw(`SELECT count(*) FROM order_items WHERE sku_id = ?`, skuID).Scan(&items).Error)
	require.NoError(t, db.Raw(`SELECT count(*) FROM orders o
		LEFT JOIN order_items i ON i.order_id = o.id
		LEFT JOIN inventory_transactions it ON it.order_id = o.id AND it.reason = 'ORDER_CREATED'
		WHERE o.idempotency_key LIKE ? AND (i.id IS NULL OR it.id IS NULL)`, keyPattern).Scan(&halfOrders).Error)
	require.NoError(t, db.Raw(`SELECT count(*), COALESCE(sum(change), 0) FROM inventory_transactions
		WHERE sku_id = ? AND reason = 'ORDER_CREATED'`, skuID).Row().Scan(&ledgerCount, &ledgerSum))
	require.Equal(t, wantOrders, orders)
	require.Equal(t, wantOrders, items)
	require.Zero(t, halfOrders)
	require.Equal(t, wantOrders, ledgerCount)
	require.Equal(t, wantLedger, ledgerSum)
}
