package promotion

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

func TestPostgresVideoProductConcurrency(t *testing.T) {
	if os.Getenv("TOKMALL_RUN_PG_INTEGRATION") != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn)
	db := isolatedPostgres(t, dsn)
	svc := NewService(NewRepository(db))

	t.Run("concurrent distinct links cannot exceed active limit", func(t *testing.T) {
		author, video, products := seedFixture(t, db, "limit", 20)
		var success, limited, unexpected atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, productID := range products {
			wg.Add(1)
			go func(productID int64) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := svc.Link(ctx, LinkParams{AuthorID: author, VideoID: video, ProductID: productID})
				switch {
				case err == nil:
					success.Add(1)
				case errors.Is(err, ErrActiveLimit):
					limited.Add(1)
				default:
					unexpected.Add(1)
				}
			}(productID)
		}
		close(start)
		wg.Wait()
		require.Equal(t, int64(5), success.Load())
		require.Equal(t, int64(15), limited.Load())
		require.Zero(t, unexpected.Load())
		var count int64
		require.NoError(t, db.Raw("SELECT count(*) FROM video_products WHERE video_id=? AND status='ACTIVE'", video).Scan(&count).Error)
		require.Equal(t, int64(5), count)
		var activeProductID int64
		require.NoError(t, db.Raw("SELECT product_id FROM video_products WHERE video_id=? AND status='ACTIVE' LIMIT 1", video).Scan(&activeProductID).Error)
		_, err := svc.Link(context.Background(), LinkParams{AuthorID: author, VideoID: video, ProductID: activeProductID, SortOrder: 4})
		require.NoError(t, err, "replaying an already-active relation must still work when the video is full")
	})

	t.Run("concurrent duplicate link creates one relation", func(t *testing.T) {
		author, video, products := seedFixture(t, db, "duplicate", 1)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 20)
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := svc.Link(ctx, LinkParams{AuthorID: author, VideoID: video, ProductID: products[0], SortOrder: 2})
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		var count int64
		require.NoError(t, db.Raw("SELECT count(*) FROM video_products WHERE video_id=? AND product_id=?", video, products[0]).Scan(&count).Error)
		require.Equal(t, int64(1), count)
	})

	t.Run("ownership and sellability failures leave no relation", func(t *testing.T) {
		author, video, products := seedFixture(t, db, "hidden", 1)
		other, _, otherProducts := seedFixture(t, db, "other", 1)
		cases := []LinkParams{
			{AuthorID: other, VideoID: video, ProductID: products[0]},
			{AuthorID: author, VideoID: video, ProductID: otherProducts[0]},
		}
		for _, params := range cases {
			_, err := svc.Link(context.Background(), params)
			require.ErrorIs(t, err, ErrNotFound)
		}
		require.NoError(t, db.Exec("UPDATE videos SET status='BLOCKED' WHERE id=?", video).Error)
		_, err := svc.Link(context.Background(), LinkParams{AuthorID: author, VideoID: video, ProductID: products[0]})
		require.ErrorIs(t, err, ErrNotFound)
		require.NoError(t, db.Exec("UPDATE videos SET status='PUBLISHED' WHERE id=?", video).Error)
		require.NoError(t, db.Exec("UPDATE products SET status='OFF_SALE' WHERE id=?", products[0]).Error)
		_, err = svc.Link(context.Background(), LinkParams{AuthorID: author, VideoID: video, ProductID: products[0]})
		require.ErrorIs(t, err, ErrNotFound)
		var count int64
		require.NoError(t, db.Raw("SELECT count(*) FROM video_products WHERE video_id=?", video).Scan(&count).Error)
		require.Zero(t, count)
	})

	t.Run("author can unlink after product goes off sale or video is blocked", func(t *testing.T) {
		author, video, products := seedFixture(t, db, "unlink-unavailable", 1)
		_, err := svc.Link(context.Background(), LinkParams{AuthorID: author, VideoID: video, ProductID: products[0]})
		require.NoError(t, err)
		require.NoError(t, db.Exec("UPDATE products SET status='OFF_SALE' WHERE id=?", products[0]).Error)
		require.NoError(t, db.Exec("UPDATE videos SET status='BLOCKED' WHERE id=?", video).Error)
		result, err := svc.Unlink(context.Background(), author, video, products[0])
		require.NoError(t, err)
		require.Equal(t, StatusDisabled, result.Status)
	})
}

func seedFixture(t *testing.T, db *gorm.DB, label string, n int) (int64, int64, []int64) {
	t.Helper()
	prefix := label + "-" + uuid.NewString()
	var author, video int64
	require.NoError(t, db.Raw(`INSERT INTO users(email,username,password_hash,role) VALUES(?,?,'hash','MERCHANT') RETURNING id`, prefix+"@example.test", prefix).Scan(&author).Error)
	require.NoError(t, db.Raw(`INSERT INTO videos(author_id,title,object_key,status,published_at,media_probe_status) VALUES(?,'video',?,'PUBLISHED',now(),'SKIPPED') RETURNING id`, author, prefix+".mp4").Scan(&video).Error)
	products := make([]int64, 0, n)
	for i := range n {
		var productID, skuID int64
		require.NoError(t, db.Raw(`INSERT INTO products(merchant_id,title,status) VALUES(?,?,'ON_SALE') RETURNING id`, author, fmt.Sprintf("%s-product-%d", prefix, i)).Scan(&productID).Error)
		require.NoError(t, db.Raw(`INSERT INTO skus(product_id,spec,price,status) VALUES(?,'default',100,'ACTIVE') RETURNING id`, productID).Scan(&skuID).Error)
		products = append(products, productID)
	}
	return author, video, products
}

func isolatedPostgres(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := "tokmall_promotion_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)
	scoped, err := withSearchPath(dsn, schema)
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(scoped), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(30)
	t.Cleanup(func() {
		_ = sqlDB.Close()
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		if x, e := admin.DB(); e == nil {
			_ = x.Close()
		}
	})
	applyMigrations(t, db)
	return db
}
func withSearchPath(dsn, schema string) (string, error) {
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
func applyMigrations(t *testing.T, db *gorm.DB) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	sort.Strings(paths)
	for _, p := range paths {
		body, err := os.ReadFile(p)
		require.NoError(t, err)
		require.NoErrorf(t, db.Exec(string(body)).Error, "apply %s", filepath.Base(p))
	}
}
