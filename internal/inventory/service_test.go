package inventory

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeRepository struct {
	getBySkuIDFn    func(context.Context, int64) (*Inventory, error)
	findSkuOwnerFn  func(context.Context, int64) (*SkuOwner, error)
	adjustFn        func(context.Context, *InventoryTransaction) error
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

func (f *fakeRepository) Adjust(ctx context.Context, tx *InventoryTransaction) error {
	f.adjustCalls++
	f.lastTransaction = tx
	if f.adjustFn == nil {
		return nil
	}
	return f.adjustFn(ctx, tx)
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

	t.Run("missing row reads as zero", func(t *testing.T) {
		repo := &fakeRepository{
			findSkuOwnerFn: func(context.Context, int64) (*SkuOwner, error) { return testOwner(), nil },
			getBySkuIDFn:   func(context.Context, int64) (*Inventory, error) { return nil, ErrNotFound },
		}
		service := NewService(repo)

		got, err := service.GetStock(context.Background(), 7, 101, 11)

		if err != nil {
			t.Fatalf("GetStock() error = %v", err)
		}
		if got.Quantity != 0 {
			t.Errorf("quantity = %d, want 0 (惰性初始化读侧语义)", got.Quantity)
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
		if tx.Change != -5 || tx.Reason != ReasonAdjust || tx.BizID != "adjust:key-1" {
			t.Errorf("transaction = %#v, want change=-5 reason=ADJUST biz_id=adjust:key-1", tx)
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
			adjustFn: func(context.Context, *InventoryTransaction) error { return ErrDuplicateBiz },
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
			adjustFn:       func(context.Context, *InventoryTransaction) error { return ErrInsufficientStock },
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
