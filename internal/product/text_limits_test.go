package product

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var errStopAtRepository = errors.New("reached repository")

// 合同（OpenAPI maxLength）按字符计；服务端必须同口径，否则中文用户会在合同允许的长度内被拒。
// 通过判定的标准：超限返回 ErrInvalidParam 且不触达仓储；合法值会进入仓储（仓储返回哨兵错误）。
func TestProductTextLimitsCountCharacters(t *testing.T) {
	cjk := func(n int) string { return strings.Repeat("中", n) }
	ctx := context.Background()

	t.Run("product title and description", func(t *testing.T) {
		repo := &fakeProductRepository{createProductFn: func(context.Context, *Product) error { return errStopAtRepository }}
		svc := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := svc.CreateProduct(ctx, CreateProductParams{MerchantID: 7, Title: cjk(200), Description: cjk(2000)})
		require.ErrorIs(t, err, errStopAtRepository)

		_, err = svc.CreateProduct(ctx, CreateProductParams{MerchantID: 7, Title: cjk(201)})
		require.ErrorIs(t, err, ErrInvalidParam)
		_, err = svc.CreateProduct(ctx, CreateProductParams{MerchantID: 7, Title: "ok", Description: cjk(2001)})
		require.ErrorIs(t, err, ErrInvalidParam)
	})

	t.Run("sku spec", func(t *testing.T) {
		repo := &fakeProductRepository{
			findProductByIDFn: func(context.Context, int64) (*Product, error) { return testProduct(), nil },
			createSKUFn:       func(context.Context, *SKU) error { return errStopAtRepository },
		}
		svc := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := svc.AddSKU(ctx, AddSKUParams{ProductID: 101, MerchantID: 7, Spec: cjk(200), PriceCents: 100})
		require.ErrorIs(t, err, errStopAtRepository)

		_, err = svc.AddSKU(ctx, AddSKUParams{ProductID: 101, MerchantID: 7, Spec: cjk(201), PriceCents: 100})
		require.ErrorIs(t, err, ErrInvalidParam)
	})

	t.Run("search keyword", func(t *testing.T) {
		repo := &fakeProductRepository{
			listProductsFn: func(context.Context, ProductFilter, int64, int) ([]Product, error) {
				return nil, errStopAtRepository
			},
		}
		svc := NewService(repo, fakeProductURLProvider{}, &fakeImageValidator{})

		_, err := svc.SearchProducts(ctx, cjk(64), 0, 20)
		require.ErrorIs(t, err, errStopAtRepository)

		_, err = svc.SearchProducts(ctx, cjk(65), 0, 20)
		require.ErrorIs(t, err, ErrInvalidParam)
	})
}
