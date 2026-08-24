package product

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidParam            = errors.New("invalid param")
	ErrInvalidStatusTransition = errors.New("invalid product status transition")
	// 临时占位：你补完业务逻辑后，这个错误不应再被 service 返回。
	ErrNotImplemented = errors.New("not implemented")
)

// Repository 由 service 定义，GormRepository 隐式满足。
type Repository interface {
	CreateProduct(ctx context.Context, p *Product) error
	FindProductByID(ctx context.Context, id int64) (*Product, error)
	UpdateProductStatus(ctx context.Context, id, merchantID int64, from, to ProductStatus) error
	ListProducts(ctx context.Context, filter ProductFilter, beforeID int64, limit int) ([]Product, error)
}

// CoverURLProvider 是 storage.Client 的最小投影：数据库只存 CoverKey，
// service 在返回 DTO 时把 key 转成短时效的 CoverURL。
type CoverURLProvider interface {
	PresignGetURL(ctx context.Context, objectKey string) (string, error)
}

type Service struct {
	repo    Repository
	storage CoverURLProvider
}

func NewService(repo Repository, storage CoverURLProvider) *Service {
	return &Service{repo: repo, storage: storage}
}

type CreateProductParams struct {
	MerchantID  int64
	Title       string
	Description string
	CoverKey    string
}

type ProductItem struct {
	ID          int64         `json:"product_id"`
	MerchantID  int64         `json:"merchant_id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	CoverURL    *string       `json:"cover_url"`
	Status      ProductStatus `json:"status"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

type ProductListParams struct {
	MerchantID int64
	Status     ProductStatus
	Keyword    string
	BeforeID   int64
	Limit      int
}

type ProductListResult struct {
	Items        []ProductItem `json:"items"`
	NextBeforeID *int64        `json:"next_before_id,omitempty"`
	HasMore      bool          `json:"has_more"`
}

func (s *Service) CreateProduct(ctx context.Context, p CreateProductParams) (*ProductItem, error) {
	if p.MerchantID <= 0 {
		return nil, ErrInvalidParam
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return nil, ErrInvalidParam
	}
	description := strings.TrimSpace(p.Description)
	product := Product{
		MerchantID:  p.MerchantID,
		Title:       title,
		Description: description,
		CoverKey:    p.CoverKey,
		Status:      ProductStatusOnSale,
	}
	err := s.repo.CreateProduct(ctx, &product)
	if err != nil {
		return nil, err
	}
	return s.toProductItem(ctx, &product)
}

// GetProduct 查询商品。DELETED 商品对外应表现为 ErrNotFound。
func (s *Service) GetProduct(ctx context.Context, id int64) (*ProductItem, error) {
	if id <= 0 {
		return nil, ErrInvalidParam
	}
	product, err := s.repo.FindProductByID(ctx, id)
	if err != nil {
		return nil, err // repo包装过not found的err了，但是后续还有改进空间
	}
	if product.Status == ProductStatusDeleted {
		return nil, ErrNotFound
	}
	return s.toProductItem(ctx, product)
}

// ChangeProductStatus 读取当前状态，校验状态迁移，再用 repository 的条件更新防并发。
func (s *Service) ChangeProductStatus(ctx context.Context, merchantID, id int64, to ProductStatus) (*ProductItem, error) {
	if id <= 0 {
		return nil, ErrInvalidParam
	}
	product, err := s.repo.FindProductByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if product.MerchantID != merchantID {
		return nil, ErrNotFound
	}
	if !canTransitionProductStatus(product.Status, to) {
		return nil, ErrInvalidStatusTransition
	}
	err = s.repo.UpdateProductStatus(ctx, id, merchantID, product.Status, to)
	if err != nil {
		return nil, err
	}
	product.Status = to
	return s.toProductItem(ctx, product)
}

// ListProducts 查询商品列表。分页细节先沿用 repository 的 beforeID，后续接 HTTP 时再封装游标。
func (s *Service) ListProducts(ctx context.Context, p ProductListParams) (*ProductListResult, error) {
	if p.MerchantID <= 0 {
		return nil, ErrInvalidParam
	}
	filter := ProductFilter{
		MerchantID: p.MerchantID,
		Status:     p.Status,
		Keyword:    p.Keyword,
	}
	products, err := s.repo.ListProducts(ctx, filter, p.BeforeID, p.Limit)
	if err != nil {
		return nil, err
	}
	var items []ProductItem
	for _, product := range products {
		item, err := s.toProductItem(ctx, &product)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}

	result := ProductListResult{
		Items:        items,
		NextBeforeID: nil,
		HasMore:      false,
	}
	return &result, nil
}

func canTransitionProductStatus(from, to ProductStatus) bool {
	switch from {
	case ProductStatusOnSale:
		return to == ProductStatusOffSale || to == ProductStatusDeleted
	case ProductStatusOffSale:
		return to == ProductStatusOnSale || to == ProductStatusDeleted
	default:
		return false
	}
}

func (s *Service) toProductItem(ctx context.Context, p *Product) (*ProductItem, error) {
	if p == nil {
		return nil, nil
	}
	item := &ProductItem{
		ID:          p.ID,
		MerchantID:  p.MerchantID,
		Title:       p.Title,
		Description: p.Description,
		Status:      p.Status,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
	if p.CoverKey == "" {
		return item, nil
	}
	if s.storage == nil {
		return nil, errors.New("cover url provider is nil")
	}
	coverURL, err := s.storage.PresignGetURL(ctx, p.CoverKey)
	if err != nil {
		return nil, fmt.Errorf("presign cover url: %w", err)
	}
	item.CoverURL = &coverURL
	return item, nil
}
