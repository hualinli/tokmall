package attribution

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lihualin/tokmall/internal/feed"
	"github.com/lihualin/tokmall/internal/promotion"
	"github.com/lihualin/tokmall/internal/video"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestPostgresCardsAndPaidAttribution(t *testing.T) {
	if os.Getenv("TOKMALL_RUN_PG_INTEGRATION") != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn)
	db := attributionTestDB(t, dsn)
	ctx := context.Background()

	var authorID, buyerID, videoID, productID, skuID, orderID int64
	require.NoError(t, db.Raw(`INSERT INTO users(email,username,password_hash,role)
		VALUES ('author@example.test','author','hash','MERCHANT') RETURNING id`).Scan(&authorID).Error)
	require.NoError(t, db.Raw(`INSERT INTO users(email,username,password_hash,role)
		VALUES ('buyer@example.test','buyer','hash','USER') RETURNING id`).Scan(&buyerID).Error)
	require.NoError(t, db.Raw(`INSERT INTO videos(author_id,title,object_key,status,published_at)
		VALUES (?,'video','videos/test.mp4','PUBLISHED',now()) RETURNING id`, authorID).Scan(&videoID).Error)
	require.NoError(t, db.Raw(`INSERT INTO products(merchant_id,title,cover_key,status)
		VALUES (?,'Product','products/cover.jpg','ON_SALE') RETURNING id`, authorID).Scan(&productID).Error)
	require.NoError(t, db.Raw(`INSERT INTO skus(product_id,spec,price,status)
		VALUES (?,'S',1000,'ACTIVE') RETURNING id`, productID).Scan(&skuID).Error)
	require.NoError(t, db.Exec(`INSERT INTO video_products(video_id,author_id,product_id)
		VALUES (?,?,?)`, videoID, authorID, productID).Error)

	feedCards, err := feed.NewProductCardReader(db).ListProductCards(ctx, []int64{videoID})
	require.NoError(t, err)
	require.Len(t, feedCards, 1)
	require.Equal(t, int64(1000), feedCards[0].MinPrice)
	videoCards, err := video.NewProductCardReader(db).ListProductCards(ctx, videoID)
	require.NoError(t, err)
	require.Len(t, videoCards, 1)
	require.Equal(t, productID, videoCards[0].ProductID)

	require.NoError(t, db.Raw(`INSERT INTO orders(order_no,user_id,merchant_id,status,total_amount,
		idempotency_key,request_fingerprint,source_video_id,source_author_id,source_product_id,
		expires_at,paid_at)
		VALUES ('paid-1',?,?,'PAID',2000,'paid-1',repeat('a',64),?,?,?,now()+interval '15 minutes',now()) RETURNING id`,
		buyerID, authorID, videoID, authorID, productID).Scan(&orderID).Error)
	require.NoError(t, db.Exec(`INSERT INTO order_items(order_id,product_id,sku_id,
		merchant_id_snapshot,product_title_snapshot,sku_spec_snapshot,unit_price,quantity,line_amount)
		VALUES (?,?,?,?,'Product','S',1000,2,2000)`, orderID, productID, skuID, authorID).Error)
	require.NoError(t, db.Raw(`INSERT INTO orders(order_no,user_id,merchant_id,status,total_amount,
		idempotency_key,request_fingerprint,source_video_id,source_author_id,source_product_id,
		expires_at,paid_at)
		VALUES ('paid-2',?,?,'PAID',1000,'paid-2',repeat('c',64),?,?,?,now()+interval '15 minutes',now()) RETURNING id`,
		buyerID, authorID, videoID, authorID, productID).Scan(&orderID).Error)
	require.NoError(t, db.Exec(`INSERT INTO order_items(order_id,product_id,sku_id,
		merchant_id_snapshot,product_title_snapshot,sku_spec_snapshot,unit_price,quantity,line_amount)
		VALUES (?,?,?,?,'Product','S',1000,1,1000)`, orderID, productID, skuID, authorID).Error)
	// An unpaid sourced order and a paid organic order must not increase attributed sales.
	require.NoError(t, db.Exec(`INSERT INTO orders(order_no,user_id,merchant_id,status,total_amount,
		idempotency_key,request_fingerprint,source_video_id,source_author_id,source_product_id,expires_at)
		VALUES ('pending-1',?,?,'PENDING_PAYMENT',1000,'pending-1',repeat('b',64),?,?,?,now()+interval '15 minutes')`,
		buyerID, authorID, videoID, authorID, productID).Error)
	require.NoError(t, db.Exec(`INSERT INTO orders(order_no,user_id,merchant_id,status,total_amount,
		idempotency_key,request_fingerprint,expires_at,paid_at)
		VALUES ('organic-1',?,?,'PAID',1000,'organic-1',repeat('d',64),now()+interval '15 minutes',now())`,
		buyerID, authorID).Error)

	sales, err := NewRepository(db).Sales(ctx, authorID)
	require.NoError(t, err)
	require.Equal(t, int64(2), sales.OrderCount)
	require.Equal(t, int64(1), sales.BuyerCount)
	require.Equal(t, int64(3), sales.UnitsSold)
	require.Equal(t, int64(3000), sales.GMV)
	require.Equal(t, videoID, sales.ByVideo[0].ID)
	require.Equal(t, productID, sales.ByProduct[0].ID)

	require.NoError(t, db.Exec("UPDATE products SET status='OFF_SALE' WHERE id=?", productID).Error)
	feedCards, err = feed.NewProductCardReader(db).ListProductCards(ctx, []int64{videoID})
	require.NoError(t, err)
	require.Len(t, feedCards, 1)
	require.Equal(t, "OFF_SALE", feedCards[0].ProductStatus)
	videoCards, err = video.NewProductCardReader(db).ListProductCards(ctx, videoID)
	require.NoError(t, err)
	require.Equal(t, "OFF_SALE", videoCards[0].ProductStatus)
	promotionCards, err := promotion.NewRepository(db).ListActiveCards(ctx, videoID)
	require.NoError(t, err)
	require.Equal(t, "OFF_SALE", promotionCards[0].ProductStatus)
	sales, err = NewRepository(db).Sales(ctx, authorID)
	require.NoError(t, err)
	require.Equal(t, int64(3000), sales.GMV, "historical payment survives product status changes")
}

func attributionTestDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := "tokmall_attribution_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)
	u, err := url.Parse(dsn)
	if err == nil && u.Scheme != "" {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	sort.Strings(paths)
	for _, path := range paths {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoErrorf(t, db.Exec(string(body)).Error, "apply %s", filepath.Base(path))
	}
	return db
}
