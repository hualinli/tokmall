package order

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostgresOrderReadAndFulfillment(t *testing.T) {
	if os.Getenv(postgresIntegrationSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn)
	db := newIsolatedPostgres(t, dsn)
	buyers, skuID := seedOrderFixture(t, db, "read-fulfill", 2, 10)
	var merchantID int64
	require.NoError(t, db.Raw(`SELECT p.merchant_id FROM products p JOIN skus s ON s.product_id = p.id WHERE s.id = ?`, skuID).Scan(&merchantID).Error)
	svc := NewService(NewRepository(db))
	ctx := context.Background()
	var created []*CreateResult
	for _, key := range []string{"one", "two", "three"} {
		order, err := svc.Create(ctx, CreateParams{UserID: buyers[0], SKUID: skuID, Quantity: 1, IdempotencyKey: key})
		require.NoError(t, err)
		created = append(created, order)
	}
	other, err := svc.Create(ctx, CreateParams{UserID: buyers[1], SKUID: skuID, Quantity: 1, IdempotencyKey: "other"})
	require.NoError(t, err)

	page, err := svc.List(ctx, buyers[0], 0, 2, false)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, created[2].OrderID, page.Items[0].OrderID)
	require.NotNil(t, page.NextBeforeID)
	page2, err := svc.List(ctx, buyers[0], *page.NextBeforeID, 2, false)
	require.NoError(t, err)
	require.Len(t, page2.Items, 1)
	require.Equal(t, created[0].OrderID, page2.Items[0].OrderID)
	require.Nil(t, page2.NextBeforeID)
	merchantPage, err := svc.List(ctx, merchantID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, merchantPage.Items, 4)
	require.Equal(t, other.OrderID, merchantPage.Items[0].OrderID)

	_, err = svc.Get(ctx, buyers[1], created[0].OrderID, false)
	require.ErrorIs(t, err, ErrOrderNotFound)
	_, err = svc.Get(ctx, buyers[1], created[0].OrderID, true)
	require.ErrorIs(t, err, ErrOrderNotFound)
	view, err := svc.Get(ctx, merchantID, created[0].OrderID, true)
	require.NoError(t, err)
	require.Equal(t, created[0].Item.ProductTitle, view.Item.ProductTitle)

	_, err = svc.Fulfill(ctx, merchantID, created[0].OrderID)
	require.ErrorIs(t, err, ErrOrderStateConflict)
	_, err = svc.Complete(ctx, buyers[0], created[0].OrderID)
	require.ErrorIs(t, err, ErrOrderStateConflict)
	_, err = svc.Fulfill(ctx, buyers[1], created[0].OrderID)
	require.ErrorIs(t, err, ErrOrderNotFound)
	_, err = svc.Pay(ctx, PayParams{UserID: buyers[0], OrderID: created[0].OrderID,
		PaymentNo: "pay-read-fulfill", ProviderTradeNo: "trade-read-fulfill", Amount: created[0].TotalAmount})
	require.NoError(t, err)

	const workers = 12
	results := make(chan *TransitionResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.Fulfill(ctx, merchantID, created[0].OrderID)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	firsts := 0
	for err := range errs {
		require.NoError(t, err)
	}
	for result := range results {
		require.Equal(t, StatusFulfilling, result.Status)
		if !result.Replayed {
			firsts++
		}
	}
	require.Equal(t, 1, firsts)
	_, err = svc.Complete(ctx, merchantID, created[0].OrderID)
	require.True(t, errors.Is(err, ErrOrderNotFound))
	completed, err := svc.Complete(ctx, buyers[0], created[0].OrderID)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, completed.Status)
	replayed, err := svc.Complete(ctx, buyers[0], created[0].OrderID)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	view, err = svc.Get(ctx, buyers[0], created[0].OrderID, false)
	require.NoError(t, err)
	require.NotNil(t, view.FulfillingAt)
	require.NotNil(t, view.CompletedAt)
	_, err = svc.Cancel(ctx, CancelParams{UserID: buyers[0], OrderID: created[0].OrderID})
	require.ErrorIs(t, err, ErrOrderStateConflict)
	var stock int64
	require.NoError(t, db.Raw(`SELECT quantity FROM inventory WHERE sku_id = ?`, skuID).Scan(&stock).Error)
	require.Equal(t, int64(6), stock)
}
