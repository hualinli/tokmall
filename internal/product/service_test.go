package product

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeProductRepository struct {
	createProductFn       func(context.Context, *Product) error
	findProductByIDFn     func(context.Context, int64) (*Product, error)
	updateProductStatusFn func(context.Context, int64, int64, ProductStatus, ProductStatus) error
	listProductsFn        func(context.Context, ProductFilter, int64, int) ([]Product, error)
	createProductCalls    int
	findProductByIDCalls  int
	updateStatusCalls     int
	listProductsCalls     int
}

type fakeProductURLProvider struct {
	presignGetURLFn func(context.Context, string) (string, error)
}

func (f fakeProductURLProvider) PresignGetURL(ctx context.Context, objectKey string) (string, error) {
	if f.presignGetURLFn == nil {
		return "signed://" + objectKey, nil
	}
	return f.presignGetURLFn(ctx, objectKey)
}

func (f *fakeProductRepository) CreateProduct(ctx context.Context, p *Product) error {
	f.createProductCalls++
	if f.createProductFn == nil {
		return nil
	}
	return f.createProductFn(ctx, p)
}

func (f *fakeProductRepository) FindProductByID(ctx context.Context, id int64) (*Product, error) {
	f.findProductByIDCalls++
	if f.findProductByIDFn == nil {
		return nil, ErrNotFound
	}
	return f.findProductByIDFn(ctx, id)
}

func (f *fakeProductRepository) UpdateProductStatus(
	ctx context.Context,
	id, merchantID int64,
	from, to ProductStatus,
) error {
	f.updateStatusCalls++
	if f.updateProductStatusFn == nil {
		return nil
	}
	return f.updateProductStatusFn(ctx, id, merchantID, from, to)
}

func (f *fakeProductRepository) ListProducts(
	ctx context.Context,
	filter ProductFilter,
	beforeID int64,
	limit int,
) ([]Product, error) {
	f.listProductsCalls++
	if f.listProductsFn == nil {
		return nil, nil
	}
	return f.listProductsFn(ctx, filter, beforeID, limit)
}

func testProduct() *Product {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	return &Product{
		ID:          101,
		MerchantID:  7,
		Title:       "Demo product",
		Description: "description",
		CoverKey:    "products/7/cover.png",
		Status:      ProductStatusOnSale,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func TestServiceToProductItemPresignsCoverKey(t *testing.T) {
	var gotObjectKey string
	service := NewService(&fakeProductRepository{}, fakeProductURLProvider{
		presignGetURLFn: func(_ context.Context, objectKey string) (string, error) {
			gotObjectKey = objectKey
			return "https://minio.local/signed/product-cover", nil
		},
	})

	item, err := service.toProductItem(context.Background(), testProduct())

	if err != nil {
		t.Fatalf("toProductItem() error = %v", err)
	}
	if gotObjectKey != "products/7/cover.png" {
		t.Errorf("presigned object key = %q, want products/7/cover.png", gotObjectKey)
	}
	if item == nil || item.CoverURL == nil {
		t.Fatal("toProductItem() cover_url is nil")
	}
	if *item.CoverURL != "https://minio.local/signed/product-cover" {
		t.Errorf("cover_url = %q, want signed URL", *item.CoverURL)
	}
}

func TestServiceCreateProduct(t *testing.T) {
	t.Run("valid params delegate to repository and set initial status", func(t *testing.T) {
		var saved *Product
		repo := &fakeProductRepository{
			createProductFn: func(_ context.Context, p *Product) error {
				saved = p
				p.ID = 101
				return nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{})

		got, err := service.CreateProduct(context.Background(), CreateProductParams{
			MerchantID:  7,
			Title:       "  Demo product  ",
			Description: "description",
			CoverKey:    "products/7/cover.png",
		})

		if err != nil {
			t.Fatalf("CreateProduct() error = %v", err)
		}
		if repo.createProductCalls != 1 {
			t.Fatalf("CreateProduct() repository calls = %d, want 1", repo.createProductCalls)
		}
		if saved == nil {
			t.Fatal("CreateProduct() did not pass a product to repository")
		}
		if saved.Status != ProductStatusOnSale {
			t.Errorf("saved status = %q, want %q", saved.Status, ProductStatusOnSale)
		}
		if saved.Title != "Demo product" {
			t.Errorf("saved title = %q, want trimmed title", saved.Title)
		}
		if got == nil || got.ID != 101 {
			t.Errorf("result = %#v, want product 101", got)
		}
	})

	tests := []struct {
		name   string
		params CreateProductParams
	}{
		{
			name:   "merchant id is zero",
			params: CreateProductParams{Title: "valid"},
		},
		{
			name:   "title is empty",
			params: CreateProductParams{MerchantID: 7, Title: "   "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeProductRepository{}
			service := NewService(repo, fakeProductURLProvider{})

			_, err := service.CreateProduct(context.Background(), tt.params)

			if !errors.Is(err, ErrInvalidParam) {
				t.Fatalf("CreateProduct() error = %v, want ErrInvalidParam", err)
			}
			if repo.createProductCalls != 0 {
				t.Fatalf("repository calls = %d, want 0", repo.createProductCalls)
			}
		})
	}
}

func TestServiceGetProduct(t *testing.T) {
	t.Run("maps model to service result", func(t *testing.T) {
		repo := &fakeProductRepository{
			findProductByIDFn: func(_ context.Context, id int64) (*Product, error) {
				if id != 101 {
					t.Fatalf("FindProductByID() id = %d, want 101", id)
				}
				return testProduct(), nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{})

		got, err := service.GetProduct(context.Background(), 101)

		if err != nil {
			t.Fatalf("GetProduct() error = %v", err)
		}
		if got == nil || got.ID != 101 || got.Status != ProductStatusOnSale {
			t.Errorf("GetProduct() = %#v, want mapped product", got)
		}
	})

	t.Run("hides deleted product", func(t *testing.T) {
		deleted := testProduct()
		deleted.Status = ProductStatusDeleted
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return deleted, nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{})

		_, err := service.GetProduct(context.Background(), deleted.ID)

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetProduct() error = %v, want ErrNotFound", err)
		}
	})
}

func TestServiceChangeProductStatus(t *testing.T) {
	t.Run("valid transition uses compare and set", func(t *testing.T) {
		product := testProduct()
		var gotFrom, gotTo ProductStatus
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return product, nil
			},
			updateProductStatusFn: func(_ context.Context, _, _ int64, from, to ProductStatus) error {
				gotFrom, gotTo = from, to
				return nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{})

		got, err := service.ChangeProductStatus(context.Background(), product.MerchantID, product.ID, ProductStatusOffSale)

		if err != nil {
			t.Fatalf("ChangeProductStatus() error = %v", err)
		}
		if gotFrom != ProductStatusOnSale || gotTo != ProductStatusOffSale {
			t.Errorf("status update = %q -> %q, want ON_SALE -> OFF_SALE", gotFrom, gotTo)
		}
		if got == nil || got.Status != ProductStatusOffSale {
			t.Errorf("result = %#v, want OFF_SALE", got)
		}
	})

	t.Run("rejects invalid transition without update", func(t *testing.T) {
		product := testProduct()
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return product, nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{})

		_, err := service.ChangeProductStatus(context.Background(), product.MerchantID, product.ID, ProductStatusOnSale)

		if !errors.Is(err, ErrInvalidStatusTransition) {
			t.Fatalf("ChangeProductStatus() error = %v, want ErrInvalidStatusTransition", err)
		}
		if repo.updateStatusCalls != 0 {
			t.Fatalf("update calls = %d, want 0", repo.updateStatusCalls)
		}
	})

	t.Run("hides other merchants products as not found", func(t *testing.T) {
		product := testProduct()
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return product, nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{})

		_, err := service.ChangeProductStatus(context.Background(), product.MerchantID+1, product.ID, ProductStatusOffSale)

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChangeProductStatus() error = %v, want ErrNotFound", err)
		}
		if repo.updateStatusCalls != 0 {
			t.Fatalf("update calls = %d, want 0", repo.updateStatusCalls)
		}
	})

	t.Run("propagates compare and set conflict", func(t *testing.T) {
		conflict := errors.New("concurrent update")
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return testProduct(), nil
			},
			updateProductStatusFn: func(context.Context, int64, int64, ProductStatus, ProductStatus) error {
				return conflict
			},
		}
		service := NewService(repo, fakeProductURLProvider{})

		_, err := service.ChangeProductStatus(context.Background(), 7, 101, ProductStatusOffSale)

		if !errors.Is(err, conflict) {
			t.Fatalf("ChangeProductStatus() error = %v, want conflict", err)
		}
	})
}

func TestServiceListProducts(t *testing.T) {
	var gotFilter ProductFilter
	var gotBeforeID int64
	var gotLimit int
	repo := &fakeProductRepository{
		listProductsFn: func(_ context.Context, filter ProductFilter, beforeID int64, limit int) ([]Product, error) {
			gotFilter, gotBeforeID, gotLimit = filter, beforeID, limit
			first := testProduct()
			second := testProduct()
			second.ID = 100
			return []Product{*first, *second}, nil
		},
	}
	service := NewService(repo, fakeProductURLProvider{})

	got, err := service.ListProducts(context.Background(), ProductListParams{
		MerchantID: 7,
		Status:     ProductStatusOnSale,
		Keyword:    "demo",
		BeforeID:   200,
		Limit:      2,
	})

	if err != nil {
		t.Fatalf("ListProducts() error = %v", err)
	}
	if gotFilter.MerchantID != 7 || gotFilter.Status != ProductStatusOnSale || gotFilter.Keyword != "demo" {
		t.Errorf("filter = %#v, want passed filter", gotFilter)
	}
	if gotBeforeID != 200 || gotLimit != 2 {
		t.Errorf("pagination = beforeID %d, limit %d; want 200, 2", gotBeforeID, gotLimit)
	}
	if got == nil || len(got.Items) != 2 {
		t.Errorf("result = %#v, want 2 items", got)
	}
}
