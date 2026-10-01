package product

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeProductRepository struct {
	createProductFn       func(context.Context, *Product) error
	findProductByIDFn     func(context.Context, int64) (*Product, error)
	updateProductStatusFn func(context.Context, int64, int64, ProductStatus, ProductStatus) error
	listProductsFn        func(context.Context, ProductFilter, int64, int) ([]Product, error)
	createSKUFn           func(context.Context, *SKU) error
	findSKUByIDFn         func(context.Context, int64) (*SKU, error)
	updateSKUStatusFn     func(context.Context, int64, SKUStatus, SKUStatus) error
	listSKUsByProductFn   func(context.Context, int64) ([]SKU, error)
	listSKUsCalls         int
	createProductCalls    int
	findProductByIDCalls  int
	updateStatusCalls     int
	listProductsCalls     int
	createSKUCalls        int
	findSKUByIDCalls      int
	updateSKUStatusCalls  int
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

// fakeImageValidator 封面上传凭证校验假实现，默认放行。
type fakeImageValidator struct {
	err error
}

func (f *fakeImageValidator) ValidateImage(_ context.Context, _ int64, _ string) error {
	return f.err
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

func testSKU() *SKU {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	return &SKU{
		ID:        11,
		ProductID: 101,
		Spec:      "黑色 / L",
		Price:     9900,
		Status:    SKUStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func (f *fakeProductRepository) CreateSKU(ctx context.Context, s *SKU) error {
	f.createSKUCalls++
	if f.createSKUFn == nil {
		return nil
	}
	return f.createSKUFn(ctx, s)
}

func (f *fakeProductRepository) FindSKUByID(ctx context.Context, id int64) (*SKU, error) {
	f.findSKUByIDCalls++
	if f.findSKUByIDFn == nil {
		return nil, ErrNotFound
	}
	return f.findSKUByIDFn(ctx, id)
}

func (f *fakeProductRepository) UpdateSKUStatus(ctx context.Context, id int64, from, to SKUStatus) error {
	f.updateSKUStatusCalls++
	if f.updateSKUStatusFn == nil {
		return nil
	}
	return f.updateSKUStatusFn(ctx, id, from, to)
}

func (f *fakeProductRepository) ListSKUsByProduct(ctx context.Context, productID int64) ([]SKU, error) {
	f.listSKUsCalls++
	if f.listSKUsByProductFn == nil {
		return nil, nil
	}
	return f.listSKUsByProductFn(ctx, productID)
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
	}, &fakeImageValidator{})

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
	t.Run("cover key must pass ticket validation", func(t *testing.T) {
		repo := &fakeProductRepository{}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{
			err: errors.New("ticket invalid"),
		})

		_, err := service.CreateProduct(context.Background(), CreateProductParams{
			MerchantID: 7,
			Title:      "valid",
			CoverKey:   "covers/7/forged.png",
		})

		if !errors.Is(err, ErrInvalidCoverKey) {
			t.Fatalf("error = %v, want ErrInvalidCoverKey", err)
		}
		if repo.createProductCalls != 0 {
			t.Fatalf("repository calls = %d, want 0", repo.createProductCalls)
		}
	})

	t.Run("empty cover key skips validation", func(t *testing.T) {
		repo := &fakeProductRepository{}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := service.CreateProduct(context.Background(), CreateProductParams{
			MerchantID: 7,
			Title:      "valid",
		})

		if err != nil {
			t.Fatalf("CreateProduct() error = %v", err)
		}
	})

	t.Run("valid params delegate to repository and set initial status", func(t *testing.T) {
		var saved *Product
		repo := &fakeProductRepository{
			createProductFn: func(_ context.Context, p *Product) error {
				saved = p
				p.ID = 101
				return nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

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
			service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

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
			listSKUsByProductFn: func(context.Context, int64) ([]SKU, error) {
				active := testSKU()
				disabled := testSKU()
				disabled.ID = 12
				disabled.Status = SKUStatusDisabled
				return []SKU{*active, *disabled}, nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		got, err := service.GetProduct(context.Background(), 101)

		if err != nil {
			t.Fatalf("GetProduct() error = %v", err)
		}
		if got == nil || got.ID != 101 || got.Status != ProductStatusOnSale {
			t.Errorf("GetProduct() = %#v, want mapped product", got)
		}
		// 详情只暴露在售 SKU，DISABLED 对外不可见
		if len(got.SKUs) != 1 || got.SKUs[0].ID != 11 {
			t.Errorf("skus = %#v, want only active sku 11", got.SKUs)
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
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := service.GetProduct(context.Background(), deleted.ID)

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetProduct() error = %v, want ErrNotFound", err)
		}
	})
}

func TestServiceAddSKU(t *testing.T) {
	t.Run("valid params create active sku", func(t *testing.T) {
		var saved *SKU
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return testProduct(), nil // merchant 7, ON_SALE
			},
			createSKUFn: func(_ context.Context, s *SKU) error {
				saved = s
				s.ID = 11
				return nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		got, err := service.AddSKU(context.Background(), AddSKUParams{
			ProductID: 101, MerchantID: 7, Spec: " 黑色 / L ", PriceCents: 9900,
		})

		if err != nil {
			t.Fatalf("AddSKU() error = %v", err)
		}
		if saved == nil || saved.Status != SKUStatusActive || saved.Price != 9900 {
			t.Errorf("saved = %#v, want ACTIVE sku with price 9900", saved)
		}
		if saved.Spec != "黑色 / L" {
			t.Errorf("spec = %q, want trimmed", saved.Spec)
		}
		if got == nil || got.ID != 11 {
			t.Errorf("result = %#v, want sku 11", got)
		}
	})

	tests := []struct {
		name    string
		setup   func(repo *fakeProductRepository)
		params  AddSKUParams
		wantErr error
	}{
		{
			name:    "price must be positive",
			params:  AddSKUParams{ProductID: 101, MerchantID: 7, PriceCents: 0},
			wantErr: ErrInvalidParam,
		},
		{
			name: "other merchant's product hides as not found",
			setup: func(repo *fakeProductRepository) {
				repo.findProductByIDFn = func(context.Context, int64) (*Product, error) {
					return testProduct(), nil
				}
			},
			params:  AddSKUParams{ProductID: 101, MerchantID: 8, PriceCents: 100},
			wantErr: ErrNotFound,
		},
		{
			name: "deleted product rejects new skus",
			setup: func(repo *fakeProductRepository) {
				repo.findProductByIDFn = func(context.Context, int64) (*Product, error) {
					p := testProduct()
					p.Status = ProductStatusDeleted
					return p, nil
				}
			},
			params:  AddSKUParams{ProductID: 101, MerchantID: 7, PriceCents: 100},
			wantErr: ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeProductRepository{}
			if tt.setup != nil {
				tt.setup(repo)
			}
			service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

			_, err := service.AddSKU(context.Background(), tt.params)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestServiceChangeSKUStatus(t *testing.T) {
	t.Run("valid transition uses compare and set", func(t *testing.T) {
		var gotFrom, gotTo SKUStatus
		repo := &fakeProductRepository{
			findSKUByIDFn: func(context.Context, int64) (*SKU, error) { return testSKU(), nil },
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return testProduct(), nil
			},
			updateSKUStatusFn: func(_ context.Context, _ int64, from, to SKUStatus) error {
				gotFrom, gotTo = from, to
				return nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		got, err := service.ChangeSKUStatus(context.Background(), 7, 101, 11, SKUStatusDisabled)

		if err != nil {
			t.Fatalf("ChangeSKUStatus() error = %v", err)
		}
		if gotFrom != SKUStatusActive || gotTo != SKUStatusDisabled {
			t.Errorf("transition = %q -> %q, want ACTIVE -> DISABLED", gotFrom, gotTo)
		}
		if got == nil || got.Status != SKUStatusDisabled {
			t.Errorf("result = %#v, want DISABLED", got)
		}
	})

	t.Run("sku under different product is not found", func(t *testing.T) {
		repo := &fakeProductRepository{
			findSKUByIDFn: func(context.Context, int64) (*SKU, error) { return testSKU(), nil },
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := service.ChangeSKUStatus(context.Background(), 7, 999, 11, SKUStatusDisabled)

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		if repo.updateSKUStatusCalls != 0 {
			t.Fatalf("update calls = %d, want 0", repo.updateSKUStatusCalls)
		}
	})

	t.Run("other merchant is not found", func(t *testing.T) {
		repo := &fakeProductRepository{
			findSKUByIDFn: func(context.Context, int64) (*SKU, error) { return testSKU(), nil },
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return testProduct(), nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := service.ChangeSKUStatus(context.Background(), 8, 101, 11, SKUStatusDisabled)

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		if repo.updateSKUStatusCalls != 0 {
			t.Fatalf("update calls = %d, want 0", repo.updateSKUStatusCalls)
		}
	})

	t.Run("rejects invalid transition without update", func(t *testing.T) {
		repo := &fakeProductRepository{
			findSKUByIDFn: func(context.Context, int64) (*SKU, error) { return testSKU(), nil },
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return testProduct(), nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := service.ChangeSKUStatus(context.Background(), 7, 101, 11, SKUStatusActive)

		if !errors.Is(err, ErrInvalidStatusTransition) {
			t.Fatalf("error = %v, want ErrInvalidStatusTransition", err)
		}
		if repo.updateSKUStatusCalls != 0 {
			t.Fatalf("update calls = %d, want 0", repo.updateSKUStatusCalls)
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
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

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
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

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
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

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
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

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
	service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

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
	// 多取一条探测 has_more
	if gotBeforeID != 200 || gotLimit != 3 {
		t.Errorf("pagination = beforeID %d, limit %d; want 200, 3", gotBeforeID, gotLimit)
	}
	if got == nil || len(got.Items) != 2 {
		t.Errorf("result = %#v, want 2 items", got)
	}
}

func TestServiceListPagination(t *testing.T) {
	t.Run("has_more true with next_before_id", func(t *testing.T) {
		repo := &fakeProductRepository{
			listProductsFn: func(_ context.Context, _ ProductFilter, _ int64, limit int) ([]Product, error) {
				products := make([]Product, 0, limit)
				for i := 0; i < limit; i++ {
					p := testProduct()
					p.ID = int64(100 - i)
					products = append(products, *p)
				}
				return products, nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		got, err := service.ListProducts(context.Background(), ProductListParams{MerchantID: 7, Limit: 2})

		if err != nil {
			t.Fatalf("ListProducts() error = %v", err)
		}
		if !got.HasMore {
			t.Error("HasMore = false, want true")
		}
		if got.NextBeforeID == nil || *got.NextBeforeID != 99 {
			t.Errorf("NextBeforeID = %v, want 99", got.NextBeforeID)
		}
		if len(got.Items) != 2 {
			t.Errorf("items = %d, want 2 (limit 截断)", len(got.Items))
		}
	})

	t.Run("last page clears cursor", func(t *testing.T) {
		repo := &fakeProductRepository{
			listProductsFn: func(_ context.Context, _ ProductFilter, _ int64, _ int) ([]Product, error) {
				only := testProduct()
				return []Product{*only}, nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		got, err := service.SearchProducts(context.Background(), "demo", 0, 20)

		if err != nil {
			t.Fatalf("SearchProducts() error = %v", err)
		}
		if got.HasMore || got.NextBeforeID != nil {
			t.Errorf("HasMore/NextBeforeID = %v/%v, want false/nil", got.HasMore, got.NextBeforeID)
		}
	})

	t.Run("limit normalized", func(t *testing.T) {
		var gotLimit int
		repo := &fakeProductRepository{
			listProductsFn: func(_ context.Context, _ ProductFilter, _ int64, limit int) ([]Product, error) {
				gotLimit = limit
				return nil, nil
			},
		}
		service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		if _, err := service.ListProducts(context.Background(), ProductListParams{MerchantID: 7}); err != nil {
			t.Fatalf("default limit: %v", err)
		}
		if gotLimit != defaultListLimit+1 {
			t.Errorf("default limit = %d, want %d", gotLimit, defaultListLimit+1)
		}
		if _, err := service.ListProducts(context.Background(), ProductListParams{MerchantID: 7, Limit: 9999}); err != nil {
			t.Fatalf("over max limit: %v", err)
		}
		if gotLimit != maxListLimit+1 {
			t.Errorf("capped limit = %d, want %d", gotLimit, maxListLimit+1)
		}
	})
}

func TestServiceSearchProducts(t *testing.T) {
	var gotFilter ProductFilter
	repo := &fakeProductRepository{
		listProductsFn: func(_ context.Context, filter ProductFilter, _ int64, _ int) ([]Product, error) {
			gotFilter = filter
			only := testProduct()
			return []Product{*only}, nil
		},
	}
	service := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

	got, err := service.SearchProducts(context.Background(), "  demo  ", 50, 10)

	if err != nil {
		t.Fatalf("SearchProducts() error = %v", err)
	}
	// 搜索强制只看 ON_SALE，且不限定商家；keyword 去除首尾空白
	if gotFilter.Status != ProductStatusOnSale || gotFilter.MerchantID != 0 {
		t.Errorf("filter = %#v, want ON_SALE without merchant", gotFilter)
	}
	if gotFilter.Keyword != "demo" {
		t.Errorf("keyword = %q, want trimmed \"demo\"", gotFilter.Keyword)
	}
	if got == nil || len(got.Items) != 1 {
		t.Errorf("result = %#v, want 1 item", got)
	}

	t.Run("rejects invalid status filter", func(t *testing.T) {
		service := NewService(&fakeProductRepository{}, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := service.ListProducts(context.Background(), ProductListParams{
			MerchantID: 7,
			Status:     ProductStatusDeleted,
		})

		if !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("error = %v, want ErrInvalidParam", err)
		}
	})
}

// TestEscapeLikePattern 锁住"参数化只防注入、不防通配符"这一点：
// keyword=% 必须变成字面量，而不是命中全部商品。
func TestEscapeLikePattern(t *testing.T) {
	cases := map[string]string{
		"keyboard":   `keyboard`,
		"%":          `\%`,
		"_":          `\_`,
		"a%b_c":      `a\%b\_c`,
		`back\slash`: `back\\slash`,
	}
	for input, want := range cases {
		require.Equal(t, want, escapeLikePattern(input), "input=%q", input)
	}
}

// TestAddSKURejectsOverlongSpec 与 CreateProduct 的长度上限同源：
// TEXT 列没有数据库级约束，必须在服务端挡住资源放大。
func TestAddSKURejectsOverlongSpec(t *testing.T) {
	repo := &fakeProductRepository{findProductByIDFn: func(context.Context, int64) (*Product, error) {
		return &Product{ID: 1, MerchantID: 5, Title: "t", Status: ProductStatusOnSale}, nil
	}}
	svc := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

	_, err := svc.AddSKU(context.Background(), AddSKUParams{
		ProductID: 1, MerchantID: 5, PriceCents: 100, Spec: strings.Repeat("s", maxSKUSpecLength+1),
	})
	require.ErrorIs(t, err, ErrInvalidParam)
}

// TestGetProductHidesSKUsWhenOffSale：下架商品详情仍可见（历史引用/分享链接），
// 但不能给出"可购买"的外观——SKU 列表必须为空，否则用户点了下单才在服务端拿到 404。
func TestGetProductHidesSKUsWhenOffSale(t *testing.T) {
	for _, status := range []ProductStatus{ProductStatusOffSale} {
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) {
				return &Product{ID: 7, MerchantID: 5, Title: "t", Status: status}, nil
			},
			listSKUsByProductFn: func(context.Context, int64) ([]SKU, error) {
				return []SKU{{ID: 1, ProductID: 7, Price: 100, Status: SKUStatusActive}}, nil
			},
		}
		svc := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		detail, err := svc.GetProduct(context.Background(), 7)
		require.NoError(t, err)
		require.Equal(t, status, detail.Status)
		require.Empty(t, detail.SKUs, "下架商品不得返回可售 SKU")
		require.Zero(t, repo.listSKUsCalls, "下架商品不应查询 SKU")
	}
}

// fakeExistenceGuard 记录缓存穿透防护的调用。
type fakeExistenceGuard struct {
	reject bool
	added  []int64
	calls  int
}

func (g *fakeExistenceGuard) MayExist(context.Context, string, int64) (bool, error) {
	g.calls++
	return !g.reject, nil
}
func (g *fakeExistenceGuard) MarkMissing(context.Context, string, int64) {}
func (g *fakeExistenceGuard) Add(_ string, id int64)                     { g.added = append(g.added, id) }

// TestCreateProductRegistersIDInGuard 锁死一个真实踩过的坑：
// 布隆过滤器是"定期全量重建 + 创建时增量登记"的组合，漏了后者就会出现
// "商品创建成功、紧接着查详情 404"（实测：GET /products/{新 id} 返回 404，
// 直到下一次重建）。这个用例保证创建路径一定会登记 ID。
func TestCreateProductRegistersIDInGuard(t *testing.T) {
	repo := &fakeProductRepository{createProductFn: func(_ context.Context, p *Product) error {
		p.ID = 4242
		return nil
	}}
	guard := &fakeExistenceGuard{}
	svc := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})
	svc.SetExistenceGuard(guard)

	item, err := svc.CreateProduct(context.Background(), CreateProductParams{
		MerchantID: 7, Title: "新商品",
	})
	require.NoError(t, err)
	require.Equal(t, int64(4242), item.ID)
	require.Equal(t, []int64{4242}, guard.added, "新建商品必须立刻登记进布隆过滤器")
}

// TestGetProductShortCircuitsWhenGuardRejects：过滤器说"一定不存在"时不查库。
func TestGetProductShortCircuitsWhenGuardRejects(t *testing.T) {
	repo := &fakeProductRepository{}
	guard := &fakeExistenceGuard{reject: true}
	svc := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})
	svc.SetExistenceGuard(guard)

	_, err := svc.GetProduct(context.Background(), 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.Zero(t, repo.findProductByIDCalls, "被防护层拒绝时不应查询数据库")
}
