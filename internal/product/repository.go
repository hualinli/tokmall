package product

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

var ErrNotFound = errors.New("record not found")
var ErrStatusConflict = errors.New("status transition not allowed or already applied")

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

func (r *GormRepository) CreateProduct(ctx context.Context, p *Product) error {
	return r.db.WithContext(ctx).Create(p).Error
}

func (r *GormRepository) FindProductByID(ctx context.Context, id int64) (*Product, error) {
	var p Product
	if err := r.db.WithContext(ctx).First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

// merchantID 作为写入前置条件：归属校验由 service 决策，这里保证并发下的原子兜底。
func (r *GormRepository) UpdateProductStatus(
	ctx context.Context,
	id, merchantID int64,
	from ProductStatus,
	to ProductStatus,
) error {
	res := r.db.WithContext(ctx).
		Model(&Product{}).
		Where("id = ? AND merchant_id = ? AND status = ?", id, merchantID, string(from)).
		Update("status", string(to))

	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStatusConflict
	}
	return nil
}

type ProductFilter struct {
	MerchantID int64
	Status     ProductStatus // 为空表示不过滤
	Keyword    string
}

func (r *GormRepository) ListProducts(
	ctx context.Context,
	filter ProductFilter,
	beforeID int64,
	limit int,
) ([]Product, error) {
	q := r.db.WithContext(ctx)
	if filter.MerchantID > 0 {
		q = q.Where("merchant_id = ?", filter.MerchantID)
	}

	if filter.Status != "" {
		q = q.Where("status = ?", string(filter.Status))
	} else {
		// 不指定状态时排除软删商品：列表（商家/搜索）都不应翻出 DELETED
		q = q.Where("status <> ?", string(ProductStatusDeleted))
	}

	if filter.Keyword != "" {
		q = q.Where("title ILIKE ?", "%"+filter.Keyword+"%")
	}

	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}

	var products []Product
	err := q.
		Order("id DESC").
		Limit(limit).
		Find(&products).Error
	return products, err
}

func (r *GormRepository) CreateSKU(ctx context.Context, s *SKU) error {
	return r.db.WithContext(ctx).Create(s).Error
}

func (r *GormRepository) FindSKUByID(ctx context.Context, id int64) (*SKU, error) {
	var s SKU
	if err := r.db.WithContext(ctx).First(&s, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &s, nil
}

func (r *GormRepository) UpdateSKUStatus(
	ctx context.Context,
	id int64,
	from SKUStatus,
	to SKUStatus,
) error {
	res := r.db.WithContext(ctx).
		Model(&SKU{}).
		Where("id = ? AND status = ?", id, string(from)).
		Update("status", string(to))

	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStatusConflict
	}
	return nil
}
func (r *GormRepository) ListSKUsByProduct(ctx context.Context, productID int64) ([]SKU, error) {
	var skus []SKU
	err := r.db.WithContext(ctx).
		Where("product_id = ?", productID).
		Order("id ASC").
		Find(&skus).Error
	return skus, err
}
