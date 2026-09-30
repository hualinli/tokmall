package inventory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRepository struct {
	mu              sync.Mutex
	getBySkuIDFn    func(context.Context, int64) (*Inventory, error)
	findSkuOwnerFn  func(context.Context, int64) (*SkuOwner, error)
	adjustFn        func(context.Context, *InventoryTransaction) (*AdjustmentResult, error)
	adjustCalls     int
	lastTransaction *InventoryTransaction
}

func (f *fakeRepository) GetBySkuID(ctx context.Context, skuID int64) (*Inventory, error) {
	if f.getBySkuIDFn == nil {
		return &Inventory{SkuID: skuID, Quantity: 10}, nil
	}
	return f.getBySkuIDFn(ctx, skuID)
}

func (f *fakeRepository) FindSkuOwner(ctx context.Context, skuID int64) (*SkuOwner, error) {
	if f.findSkuOwnerFn == nil {
		return nil, ErrNotFound
	}
	return f.findSkuOwnerFn(ctx, skuID)
}

func (f *fakeRepository) Adjust(ctx context.Context, tx *InventoryTransaction) (*AdjustmentResult, error) {
	f.mu.Lock()
	f.adjustCalls++
	f.lastTransaction = tx
	fn := f.adjustFn
	f.mu.Unlock()
	if fn == nil {
		return &AdjustmentResult{Inserted: true}, nil
	}
	return fn(ctx, tx)
}

func testOwner() *SkuOwner {
	return &SkuOwner{ProductID: 101, MerchantID: 7}
}

func TestServiceGetStock(t *testing.T) {
	t.Run("owner sees quantity", func(t *testing.T) {
		repo := &fakeRepository{
			findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
			getBySkuIDFn: func(_ context.Context, _ int64) (*Inventory, error) {
				return &Inventory{SkuID: 11, Quantity: 42, UpdatedAt: time.Now()}, nil
			},
		}
		service := NewService(repo)

		got, err := service.GetStock(context.Background(), 7, 101, 11)

		if err != nil {
			t.Fatalf("GetStock() error = %v", err)
		}
		if got.Quantity != 42 {
			t.Errorf("quantity = %d, want 42", got.Quantity)
		}
	})

	t.Run("missing row is invariant violation", func(t *testing.T) {
		repo := &fakeRepository{
			findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
			getBySkuIDFn:   func(context.Context, int64) (*Inventory, error) { return nil, ErrNotFound },
		}
		service := NewService(repo)

		got, err := service.GetStock(context.Background(), 7, 101, 11)

		if err == nil {
			t.Fatal("GetStock() error = nil, want invariant violation")
		}
		if got != nil {
			t.Fatalf("GetStock() result = %#v, want nil", got)
		}
	})

	t.Run("sku under different product is not found", func(t *testing.T) {
		repo := &fakeRepository{
			findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
		}
		service := NewService(repo)

		_, err := service.GetStock(context.Background(), 7, 999, 11)

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})
}

func TestServiceAdjustStock(t *testing.T) {
	t.Run("valid adjust writes flow with biz id", func(t *testing.T) {
		repo := &fakeRepository{
			findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
			getBySkuIDFn: func(_ context.Context, _ int64) (*Inventory, error) {
				return &Inventory{SkuID: 11, Quantity: 15}, nil
			},
		}
		service := NewService(repo)

		got, err := service.AdjustStock(context.Background(), 7, 101, 11, -5, "key-1")

		if err != nil {
			t.Fatalf("AdjustStock() error = %v", err)
		}
		if repo.adjustCalls != 1 {
			t.Fatalf("adjust calls = %d, want 1", repo.adjustCalls)
		}
		tx := repo.lastTransaction
		if tx.Change != -5 || tx.Reason != ReasonAdjust || tx.BizID != "key-1" {
			t.Errorf("transaction = %#v, want change=-5 reason=ADJUST biz_id=key-1", tx)
		}
		if got.Quantity != 15 {
			t.Errorf("quantity = %d, want refreshed 15", got.Quantity)
		}
	})

	t.Run("duplicate biz id replays as idempotent success", func(t *testing.T) {
		repo := &fakeRepository{
			findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
			getBySkuIDFn: func(_ context.Context, _ int64) (*Inventory, error) {
				return &Inventory{SkuID: 11, Quantity: 5}, nil
			},
			adjustFn: func(_ context.Context, tx *InventoryTransaction) (*AdjustmentResult, error) {
				return &AdjustmentResult{
					Existing: &InventoryTransaction{
						SkuID: tx.SkuID, Change: tx.Change, Reason: tx.Reason, BizID: tx.BizID,
					},
				}, nil
			},
		}
		service := NewService(repo)

		got, err := service.AdjustStock(context.Background(), 7, 101, 11, 5, "key-1")

		if err != nil {
			t.Fatalf("AdjustStock() error = %v (重复 key 应幂等成功)", err)
		}
		if got.Quantity != 5 {
			t.Errorf("quantity = %d, want current 5", got.Quantity)
		}
	})

	t.Run("insufficient stock propagates", func(t *testing.T) {
		repo := &fakeRepository{
			findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
			adjustFn: func(context.Context, *InventoryTransaction) (*AdjustmentResult, error) {
				return nil, ErrInsufficientStock
			},
		}
		service := NewService(repo)

		_, err := service.AdjustStock(context.Background(), 7, 101, 11, -999, "key-1")

		if !errors.Is(err, ErrInsufficientStock) {
			t.Fatalf("error = %v, want ErrInsufficientStock", err)
		}
	})

	tests := []struct {
		name    string
		change  int64
		key     string
		wantErr error
	}{
		{name: "zero change", change: 0, key: "k", wantErr: ErrInvalidParam},
		{name: "empty key", change: 1, key: "  ", wantErr: ErrInvalidParam},
		{name: "oversized key", change: 1, key: strings.Repeat("k", maxIdempotencyKeyLength+1), wantErr: ErrInvalidParam},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewService(&fakeRepository{})

			_, err := service.AdjustStock(context.Background(), 7, 101, 11, tt.change, tt.key)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestServiceAdjustStockRejectsIdempotencyPayloadConflict(t *testing.T) {
	repo := &fakeRepository{
		findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
		getBySkuIDFn: func(_ context.Context, _ int64) (*Inventory, error) {
			return &Inventory{SkuID: 11, Quantity: 5}, nil
		},
		adjustFn: func(_ context.Context, tx *InventoryTransaction) (*AdjustmentResult, error) {
			return &AdjustmentResult{
				Existing: &InventoryTransaction{
					SkuID: tx.SkuID, Change: tx.Change + 1, Reason: tx.Reason, BizID: tx.BizID,
				},
			}, nil
		},
	}
	service := NewService(repo)

	_, err := service.AdjustStock(context.Background(), 7, 101, 11, 5, "key-1")

	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestServiceAdjustStockIdempotencyIsScopedToSKU(t *testing.T) {
	var calls []*InventoryTransaction
	repo := &fakeRepository{
		findSkuOwnerFn: func(_ context.Context, skuID int64) (*SkuOwner, error) {
			return &SkuOwner{ProductID: 101, MerchantID: 7}, nil
		},
		getBySkuIDFn: func(_ context.Context, skuID int64) (*Inventory, error) {
			return &Inventory{SkuID: skuID, Quantity: 10}, nil
		},
		adjustFn: func(_ context.Context, tx *InventoryTransaction) (*AdjustmentResult, error) {
			calls = append(calls, tx)
			return &AdjustmentResult{Inserted: true}, nil
		},
	}
	service := NewService(repo)

	for _, skuID := range []int64{11, 12} {
		if _, err := service.AdjustStock(context.Background(), 7, 101, skuID, 3, "same-client-key"); err != nil {
			t.Fatalf("AdjustStock(sku=%d) error = %v", skuID, err)
		}
	}

	if len(calls) != 2 {
		t.Fatalf("adjust calls = %d, want 2 (same key is scoped by sku)", len(calls))
	}
	if calls[0].SkuID == calls[1].SkuID || calls[0].BizID != calls[1].BizID {
		t.Fatalf("transactions = %#v, want same key on two distinct skus", calls)
	}
}

func TestServiceAdjustStockConcurrentReplayOnlyOneMutation(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var applied *InventoryTransaction
	var quantity int64 = 10
	repo := &fakeRepository{
		findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
		getBySkuIDFn: func(_ context.Context, _ int64) (*Inventory, error) {
			mu.Lock()
			defer mu.Unlock()
			return &Inventory{SkuID: 11, Quantity: quantity}, nil
		},
		adjustFn: func(_ context.Context, tx *InventoryTransaction) (*AdjustmentResult, error) {
			mu.Lock()
			defer mu.Unlock()
			if applied == nil {
				copy := *tx
				applied = &copy
				quantity += tx.Change
				return &AdjustmentResult{Inserted: true}, nil
			}
			return &AdjustmentResult{Existing: applied}, nil
		},
	}
	service := NewService(repo)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.AdjustStock(ctx, 7, 101, 11, 3, "concurrent-key")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent replay error = %v", err)
		}
	}
	if quantity != 13 {
		t.Fatalf("quantity = %d, want 13 (one positive adjustment)", quantity)
	}
}

// TestAdjustStockRejectsUnboundedChange：quantity + change 在 BIGINT 上溢出会
// 让 PostgreSQL 报 22003，最终变成 500。业务上界必须挡在服务层。
func TestAdjustStockRejectsUnboundedChange(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewService(repo)
	for _, change := range []int64{maxStockChange + 1, -maxStockChange - 1} {
		_, err := svc.AdjustStock(context.Background(), 1, 2, 3, change, "key-1")
		require.ErrorIs(t, err, ErrInvalidParam, "change=%d 必须被拒绝", change)
	}
	require.Zero(t, repo.adjustCalls, "非法参数不应触达仓储层")
}
