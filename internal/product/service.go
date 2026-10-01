package product

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

var (
	ErrInvalidParam            = errors.New("invalid param")
	ErrInvalidStatusTransition = errors.New("invalid product status transition")
	// cover_key 未通过上传凭证校验（伪造/过期/文件不存在）
	ErrInvalidCoverKey = errors.New("invalid cover key")
)

const (
	defaultListLimit = 20
	maxListLimit     = 50
	// 文本字段上限：这些列是 TEXT，没有数据库级长度约束。不设上限时
	// 单行可达数 MB，列表响应、Feed 商品卡与 Redis 缓存都会被放大，
	// 属于"用合法请求做资源放大"的滥用面。
	maxProductTitleLength       = 200
	maxProductDescriptionLength = 2000
	maxSKUSpecLength            = 200
	maxKeywordLength            = 64
)

// ExistenceGuard 是缓存穿透防护的消费侧接口（实现在 internal/existence）。
type ExistenceGuard interface {
	MayExist(ctx context.Context, namespace string, id int64) (bool, error)
	MarkMissing(ctx context.Context, namespace string, id int64)
	Add(namespace string, id int64)
}

const existenceNamespace = "product"

// Repository 由 service 定义，GormRepository 隐式满足。
type Repository interface {
	CreateProduct(ctx context.Context, p *Product) error
	FindProductByID(ctx context.Context, id int64) (*Product, error)
	UpdateProductStatus(ctx context.Context, id, merchantID int64, from, to ProductStatus) error
	ListProducts(ctx context.Context, filter ProductFilter, beforeID int64, limit int) ([]Product, error)
	CreateSKU(ctx context.Context, s *SKU) error
	FindSKUByID(ctx context.Context, id int64) (*SKU, error)
	UpdateSKUStatus(ctx context.Context, id int64, from, to SKUStatus) error
	ListSKUsByProduct(ctx context.Context, productID int64) ([]SKU, error)
}

// CoverURLProvider 是 storage.Client 的最小投影：数据库只存 CoverKey，
// service 在返回 DTO 时把 key 转成短时效的 CoverURL。
type CoverURLProvider interface {
	PresignGetURL(ctx context.Context, objectKey string) (string, error)
}

// ImageTicketValidator 消费侧接口，由 video.Service.ValidateImage 满足（main 装配）：
// 校验图片上传凭证有效且文件真实存在，并原子核销。不 import video 包的类型。
type ImageTicketValidator interface {
	ValidateImage(ctx context.Context, userID int64, objectKey string) error
}

type Service struct {
	repo      Repository
	storage   CoverURLProvider
	validator ImageTicketValidator
	existence ExistenceGuard
}

// SetExistenceGuard 由装配方注入缓存穿透防护（未注入时行为与现状一致）。
func (s *Service) SetExistenceGuard(guard ExistenceGuard) { s.existence = guard }

func NewService(repo Repository, storage CoverURLProvider, validator ImageTicketValidator) *Service {
	return &Service{repo: repo, storage: storage, validator: validator}
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

// SKUItem 对外暴露的 SKU 视图（model 永不出 service）。
type SKUItem struct {
	ID        int64     `json:"sku_id"`
	ProductID int64     `json:"product_id"`
	Spec      string    `json:"spec"`
	Price     int64     `json:"price"` // 单位：分
	Status    SKUStatus `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProductDetail 商品详情 = 商品主体 + 在售 SKU 列表。
// Go 的嵌入结构体在 JSON 序列化时会展平字段。
type ProductDetail struct {
	ProductItem
	SKUs []SKUItem `json:"skus"`
}

type AddSKUParams struct {
	ProductID  int64
	MerchantID int64
	Spec       string
	PriceCents int64
}

func (s *Service) CreateProduct(ctx context.Context, p CreateProductParams) (*ProductItem, error) {
	if p.MerchantID <= 0 {
		return nil, ErrInvalidParam
	}
	title := strings.TrimSpace(p.Title)
	if title == "" || len(title) > maxProductTitleLength {
		return nil, ErrInvalidParam
	}
	description := strings.TrimSpace(p.Description)
	if len(description) > maxProductDescriptionLength {
		return nil, ErrInvalidParam
	}
	coverKey := strings.TrimSpace(p.CoverKey)
	if coverKey != "" {
		// 防伪造：封面必须来自 /videos/upload-url 发放的图片凭证（与头像同一防伪哲学），
		// 校验失败统一映射 ErrInvalidCoverKey，不依赖 video 模块的哨兵类型
		if err := s.validator.ValidateImage(ctx, p.MerchantID, coverKey); err != nil {
			slog.Warn("validate cover failed", "merchant_id", p.MerchantID, "err", err)
			return nil, ErrInvalidCoverKey
		}
	}
	product := Product{
		MerchantID:  p.MerchantID,
		Title:       title,
		Description: description,
		CoverKey:    coverKey,
		Status:      ProductStatusOnSale,
	}
	err := s.repo.CreateProduct(ctx, &product)
	if err != nil {
		return nil, err
	}
	// 新建商品必须立刻进布隆过滤器：过滤器是定期全量重建的，不在这里登记
	// 就会出现"刚创建的商品在下次重建前被判成一定不存在"——实测过这个 bug
	// （商品创建成功，紧接着查详情 404）。视频模块同理。
	if s.existence != nil {
		s.existence.Add(existenceNamespace, product.ID)
	}
	return s.toProductItem(ctx, &product)
}

// GetProduct 查询详情。可见性规则（与 feed 商品卡一致）：
//   - DELETED：对外 404，不留任何痕迹；
//   - OFF_SALE：详情仍可见（历史订单、Feed 卡片、分享链接都会引用它），
//     但**不返回 SKU**——下架商品不能给出可购买的外观，否则用户点了下单
//     才在服务端拿到 404，属于"界面承诺了做不到的事"；
//   - ON_SALE：返回 ACTIVE 的 SKU。
//
// 可见性决策收敛在 service，详情面向所有调用方（含匿名用户）。
func (s *Service) GetProduct(ctx context.Context, id int64) (*ProductDetail, error) {
	if id <= 0 {
		return nil, ErrInvalidParam
	}
	if s.existence != nil {
		if ok, guardErr := s.existence.MayExist(ctx, existenceNamespace, id); guardErr == nil && !ok {
			return nil, ErrNotFound
		}
	}
	product, err := s.repo.FindProductByID(ctx, id)
	if err != nil {
		// 只有记录不存在才写负缓存：DELETED 商品也会返回 ErrNotFound，
		// 但那是"存在但不可见"，写负缓存本身没问题（对外确实 404），
		// 因此这里统一标记；真正要避免的是把"可见性拒绝"当成不存在来缓存
		// ——商品详情没有按调用者变化的可见性，故不存在这个问题。
		if s.existence != nil && errors.Is(err, ErrNotFound) {
			s.existence.MarkMissing(ctx, existenceNamespace, id)
		}
		return nil, err
	}
	if product.Status == ProductStatusDeleted {
		return nil, ErrNotFound
	}
	item, err := s.toProductItem(ctx, product)
	if err != nil {
		return nil, err
	}
	if product.Status != ProductStatusOnSale {
		return &ProductDetail{ProductItem: *item, SKUs: []SKUItem{}}, nil
	}
	skus, err := s.repo.ListSKUsByProduct(ctx, id)
	if err != nil {
		return nil, err
	}
	skuItems := make([]SKUItem, 0, len(skus))
	for i := range skus {
		if skus[i].Status == SKUStatusActive {
			skuItems = append(skuItems, *toSKUItem(&skus[i]))
		}
	}
	return &ProductDetail{ProductItem: *item, SKUs: skuItems}, nil
}

// AddSKU 给自己的商品新增规格。价格以分为单位、必须为正——金额校验在服务端，不信任客户端。
func (s *Service) AddSKU(ctx context.Context, p AddSKUParams) (*SKUItem, error) {
	if p.ProductID <= 0 || p.MerchantID <= 0 || p.PriceCents <= 0 {
		return nil, ErrInvalidParam
	}
	product, err := s.repo.FindProductByID(ctx, p.ProductID)
	if err != nil {
		return nil, err
	}
	// 不属于自己的商品与不存在的商品同样返回 NotFound，不泄露存在性；软删商品不可加 SKU
	if product.MerchantID != p.MerchantID || product.Status == ProductStatusDeleted {
		return nil, ErrNotFound
	}
	spec := strings.TrimSpace(p.Spec)
	if len(spec) > maxSKUSpecLength {
		return nil, ErrInvalidParam
	}
	sku := &SKU{
		ProductID: p.ProductID,
		Spec:      spec,
		Price:     p.PriceCents,
		Status:    SKUStatusActive,
	}
	if err := s.repo.CreateSKU(ctx, sku); err != nil {
		return nil, err
	}
	return toSKUItem(sku), nil
}

// ChangeSKUStatus 变更 SKU 状态。归属链：SKU -> 所属商品 -> 商家本人，任一环断裂都是 404。
func (s *Service) ChangeSKUStatus(ctx context.Context, merchantID, productID, skuID int64, to SKUStatus) (*SKUItem, error) {
	if merchantID <= 0 || productID <= 0 || skuID <= 0 {
		return nil, ErrInvalidParam
	}
	switch to {
	case SKUStatusActive, SKUStatusDisabled:
	default:
		return nil, ErrInvalidParam
	}
	sku, err := s.repo.FindSKUByID(ctx, skuID)
	if err != nil {
		return nil, err
	}
	if sku.ProductID != productID {
		return nil, ErrNotFound
	}
	product, err := s.repo.FindProductByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	if product.MerchantID != merchantID {
		return nil, ErrNotFound
	}
	if !canTransitionSKUStatus(sku.Status, to) {
		return nil, ErrInvalidStatusTransition
	}
	if err := s.repo.UpdateSKUStatus(ctx, skuID, sku.Status, to); err != nil {
		return nil, err
	}
	sku.Status = to
	return toSKUItem(sku), nil
}

func canTransitionSKUStatus(from, to SKUStatus) bool {
	switch from {
	case SKUStatusActive:
		return to == SKUStatusDisabled
	case SKUStatusDisabled:
		return to == SKUStatusActive
	default:
		return false
	}
}

func toSKUItem(s *SKU) *SKUItem {
	return &SKUItem{
		ID:        s.ID,
		ProductID: s.ProductID,
		Spec:      s.Spec,
		Price:     s.Price,
		Status:    s.Status,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
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

// ListProducts 商家管理视图：自己的商品（不含 DELETED），可按状态过滤。
func (s *Service) ListProducts(ctx context.Context, p ProductListParams) (*ProductListResult, error) {
	if p.MerchantID <= 0 {
		return nil, ErrInvalidParam
	}
	switch p.Status {
	case "", ProductStatusOnSale, ProductStatusOffSale:
	default:
		return nil, ErrInvalidParam
	}
	filter := ProductFilter{
		MerchantID: p.MerchantID,
		Status:     p.Status,
		Keyword:    strings.TrimSpace(p.Keyword),
	}
	return s.listByFilter(ctx, filter, p.BeforeID, p.Limit)
}

// SearchProducts 用户侧搜索：只暴露 ON_SALE 商品，不区分商家。
// OFF_SALE/DELETED 对用户不可见——可见性判断收敛在 service，repo 只管过滤。
// 关键字长度设上限：LIKE '%kw%' 无法走索引，超长关键字只会放大扫描成本。
func (s *Service) SearchProducts(ctx context.Context, keyword string, beforeID int64, limit int) (*ProductListResult, error) {
	keyword = strings.TrimSpace(keyword)
	if len(keyword) > maxKeywordLength {
		return nil, ErrInvalidParam
	}
	filter := ProductFilter{
		Status:  ProductStatusOnSale,
		Keyword: keyword,
	}
	return s.listByFilter(ctx, filter, beforeID, limit)
}

// listByFilter 统一的分页装配：向 repo 多取一条（limit+1）探测 has_more，
// next_before_id 取本页最后一条 ID（游标语义：上一页最后一条，非当前时间）。
func (s *Service) listByFilter(ctx context.Context, filter ProductFilter, beforeID int64, limit int) (*ProductListResult, error) {
	limit = normalizeLimit(limit)
	products, err := s.repo.ListProducts(ctx, filter, beforeID, limit+1)
	if err != nil {
		return nil, err
	}
	hasMore := len(products) > limit
	if hasMore {
		products = products[:limit]
	}
	items := make([]ProductItem, 0, len(products))
	for i := range products {
		item, err := s.toProductItem(ctx, &products[i])
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	result := ProductListResult{
		Items:   items,
		HasMore: hasMore,
	}
	if hasMore {
		nextBeforeID := products[len(products)-1].ID
		result.NextBeforeID = &nextBeforeID
	}
	return &result, nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
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
		return nil, nil //nolint:nilnil // 入参为 nil 时返回 nil，调用方已判空
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
