package reconcile_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lihualin/tokmall/internal/reconcile"
	"github.com/lihualin/tokmall/internal/testsupport"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 对账的验收方式很直接：先造一份**干净的**数据证明"没错报"，
// 再逐项注入脏数据证明"能抓到"。只会报警不会分辨的对账没有价值，
// 而只会说 OK 的对账更危险。
func TestReconcileDetectsDrift(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	runner := reconcile.NewRunner(reconcile.NewRepository(db), reconcile.WithGrace(time.Minute))
	ctx := context.Background()

	authorID, buyerID, _, skuID := seedCleanFixture(t, db)

	t.Run("clean data reports no differences", func(t *testing.T) {
		report, err := runner.Run(ctx)
		require.NoError(t, err)
		require.True(t, report.OK(), "干净数据不得误报：%+v", report)
		require.Zero(t, report.TotalDifferences())
	})

	t.Run("inventory balance drift is detected", func(t *testing.T) {
		// 直接改库存余额、不写流水：模拟"有人手工改库"或补偿脚本漏写流水。
		require.NoError(t, db.Exec(`UPDATE inventory SET quantity = quantity + 7 WHERE sku_id = ?`, skuID).Error)
		report, err := runner.Run(ctx)
		require.NoError(t, err)
		require.False(t, report.OK())
		require.Positive(t, differencesOf(t, report, "inventory_balance"))
		require.NoError(t, db.Exec(`UPDATE inventory SET quantity = quantity - 7 WHERE sku_id = ?`, skuID).Error)
	})

	t.Run("projection drift is detected", func(t *testing.T) {
		// 把投影改成错的：这是"事件丢了/消费者没跑"在数据上的表现。
		require.NoError(t, db.Exec(`UPDATE author_sales_daily SET gmv = gmv + 100 WHERE author_id = ?`, authorID).Error)
		report, err := runner.Run(ctx)
		require.NoError(t, err)
		require.Positive(t, differencesOf(t, report, "author_sales_projection"))

		row := findDifference(t, report, "author_sales_projection")
		require.Equal(t, "200", toString(row["live_gmv"]), "事实侧来自订单聚合")
		require.Equal(t, "300", toString(row["projected_gmv"]), "投影侧被人为加了 100")
		require.NoError(t, db.Exec(`UPDATE author_sales_daily SET gmv = gmv - 100 WHERE author_id = ?`, authorID).Error)
	})

	t.Run("refund consistency drift is detected", func(t *testing.T) {
		// 把订单标成已退款但不改退款单：状态与售后单据必须互为证据。
		require.NoError(t, db.Exec(`UPDATE orders SET status = 'REFUNDED', refunded_at = now(),
			refund_from_status = 'PAID' WHERE user_id = ?`, buyerID).Error)
		report, err := runner.Run(ctx)
		require.NoError(t, err)
		require.Positive(t, differencesOf(t, report, "refund_order_consistency"))
		require.NoError(t, db.Exec(`UPDATE orders SET status = 'PAID', refunded_at = NULL,
			refund_from_status = NULL WHERE user_id = ?`, buyerID).Error)
	})

	t.Run("missing consumer receipt is detected and replayed", func(t *testing.T) {
		// 造一条"已发布但没有回执"的事件：模拟 broker 丢数据（Redis 无 AOF 重启）。
		eventID := uuid.New()
		require.NoError(t, db.Exec(`INSERT INTO outbox_events
			(event_id, event_type, aggregate_type, aggregate_id, partition_key, payload, status, attempts, published_at, available_at)
			VALUES (?, 'commerce.order.paid.v1', 'order', 999, 'order', '{}'::jsonb, 'PUBLISHED', 1, now() - interval '10 minutes', now() - interval '10 minutes')`,
			eventID).Error)

		report, err := runner.Run(ctx)
		require.NoError(t, err)
		require.Positive(t, differencesOf(t, report, "outbox_delivery"))

		replayed, err := runner.ReplayMissingReceipts(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, replayed)

		var status string
		require.NoError(t, db.Raw(`SELECT status FROM outbox_events WHERE event_id = ?`, eventID).Scan(&status).Error)
		require.Equal(t, "PENDING", status, "重放后必须回到待投递状态")
		require.NoError(t, db.Exec(`DELETE FROM outbox_events WHERE event_id = ?`, eventID).Error)

		report, err = runner.Run(ctx)
		require.NoError(t, err)
		require.True(t, report.OK(), "重放并清理后应恢复一致")
	})
}

func differencesOf(t *testing.T, report *reconcile.Report, name string) int {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return len(check.Differences)
		}
	}
	t.Fatalf("check %s not found in report", name)
	return 0
}

func findDifference(t *testing.T, report *reconcile.Report, name string) map[string]any {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name && len(check.Differences) > 0 {
			return check.Differences[0].Columns
		}
	}
	t.Fatalf("no difference found for check %s", name)
	return nil
}

// toString 把驱动返回的列值统一成字符串：文本列可能是 string 或 []byte，
// 数值列则是 int64/float64。测试只关心"值对不对"，不关心驱动类型。
func toString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

// seedCleanFixture 造一份内部一致的业务数据：商品/SKU/库存/订单/支付/
// 退款单/库存流水/事件与投影，全部对齐。
func seedCleanFixture(t *testing.T, db *gorm.DB) (authorID, buyerID, merchantID, skuID int64) {
	t.Helper()
	prefix := "reconcile-" + uuid.NewString()[:8]
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash)
		VALUES (?, ?, 'hash') RETURNING id`, prefix+"-author@example.test", prefix+"-author").Scan(&authorID).Error)
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash)
		VALUES (?, ?, 'hash') RETURNING id`, prefix+"-buyer@example.test", prefix+"-buyer").Scan(&buyerID).Error)
	require.NoError(t, db.Raw(`INSERT INTO users (email, username, password_hash, role)
		VALUES (?, ?, 'hash', 'MERCHANT') RETURNING id`, prefix+"-merchant@example.test", prefix+"-merchant").Scan(&merchantID).Error)

	var productID int64
	require.NoError(t, db.Raw(`INSERT INTO products (merchant_id, title, status)
		VALUES (?, ?, 'ON_SALE') RETURNING id`, merchantID, prefix+" product").Scan(&productID).Error)
	require.NoError(t, db.Raw(`INSERT INTO skus (product_id, spec, price, status)
		VALUES (?, 'default', 200, 'ACTIVE') RETURNING id`, productID).Scan(&skuID).Error)
	// 库存必须由流水建立：期初 0 + ADJUST +10 - 下单 1 = 9。
	// 直接写一个没有流水的余额，对账会（正确地）报差异。
	require.NoError(t, db.Exec(`INSERT INTO inventory (sku_id, quantity, version) VALUES (?, 9, 0)`, skuID).Error)
	require.NoError(t, db.Exec(`INSERT INTO inventory_transactions (sku_id, change, reason, biz_id)
		VALUES (?, 10, 'ADJUST', ?)`, skuID, prefix+"-seed").Error)

	var orderID int64
	paidAt := time.Now().UTC().Add(-time.Hour)
	// source_* 三列必须同时存在（orders_source_all_or_none_check），
	// 这里用真实商品 ID 与一个占位视频 ID 组成完整的归因快照。
	require.NoError(t, db.Raw(`INSERT INTO orders
		(order_no, user_id, merchant_id, status, total_amount, idempotency_key, request_fingerprint,
		 source_video_id, source_author_id, source_product_id, expires_at, paid_at)
		VALUES (?, ?, ?, 'PAID', 200, ?, repeat('a', 64), 1, ?, ?, now() + interval '30 minutes', ?) RETURNING id`,
		prefix+"-order", buyerID, merchantID, prefix+"-key", authorID, productID, paidAt).Scan(&orderID).Error)
	require.NoError(t, db.Exec(`INSERT INTO order_items
		(order_id, product_id, sku_id, merchant_id_snapshot, product_title_snapshot, sku_spec_snapshot, unit_price, quantity, line_amount)
		VALUES (?, ?, ?, ?, 'product', 'default', 200, 1, 200)`, orderID, productID, skuID, merchantID).Error)
	require.NoError(t, db.Exec(`INSERT INTO payments (payment_no, order_id, amount, status, provider_trade_no, paid_at)
		VALUES (?, ?, 200, 'SUCCESS', ?, ?)`, prefix+"-pay", orderID, prefix+"-trade", paidAt).Error)
	// 库存流水：下单扣 1（库存 10 - 1 = 9，与上面的 quantity 一致）。
	require.NoError(t, db.Exec(`INSERT INTO inventory_transactions (sku_id, order_id, change, reason, biz_id)
		VALUES (?, ?, -1, 'ORDER_CREATED', ?)`, skuID, orderID, prefix+"-order").Error)
	// 事件与投影：与订单事实一致（1 单 / 1 件 / 200 分）。
	require.NoError(t, db.Exec(`INSERT INTO outbox_events
		(event_id, event_type, aggregate_type, aggregate_id, partition_key, payload, status, attempts, published_at, available_at)
		VALUES (?, 'commerce.order.paid.v1', 'order', ?, ?, '{}'::jsonb, 'PUBLISHED', 1, now() - interval '1 hour', now() - interval '1 hour')`,
		uuid.New(), orderID, "author:"+toStringInt(authorID)).Error)
	require.NoError(t, db.Exec(`INSERT INTO consumed_events (consumer, event_id, event_type, aggregate_id)
		SELECT 'attribution-projection', event_id, event_type, aggregate_id FROM outbox_events WHERE aggregate_id = ?`, orderID).Error)
	require.NoError(t, db.Exec(`INSERT INTO author_sales_daily (author_id, stat_date, order_count, units_sold, gmv)
		VALUES (?, ?, 1, 1, 200)`, authorID, paidAt.Format("2006-01-02")).Error)
	return authorID, buyerID, merchantID, skuID
}

func toStringInt(value int64) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buf []byte
	for value > 0 {
		buf = append([]byte{digits[value%10]}, buf...)
		value /= 10
	}
	return string(buf)
}
