package product

import "time"

// ProductStatus 商品状态，由业务层校验，不依赖数据库 CHECK 约束
type ProductStatus string

const (
	ProductStatusOnSale  ProductStatus = "ON_SALE"
	ProductStatusOffSale ProductStatus = "OFF_SALE"
	ProductStatusDeleted ProductStatus = "DELETED"
)

// SKUStatus SKU 状态
type SKUStatus string

const (
	SKUStatusActive   SKUStatus = "ACTIVE"
	SKUStatusDisabled SKUStatus = "DISABLED"
)

// Product 对应 products 表
type Product struct {
	ID          int64         `gorm:"column:id;primaryKey"`
	MerchantID  int64         `gorm:"column:merchant_id"`
	Title       string        `gorm:"column:title"`
	Description string        `gorm:"column:description"`
	CoverURL    string        `gorm:"column:cover_url"`
	Status      ProductStatus `gorm:"column:status"`
	CreatedAt   time.Time     `gorm:"column:created_at"`
	UpdatedAt   time.Time     `gorm:"column:updated_at"`
}

func (Product) TableName() string {
	return "products"
}

// SKU 对应 skus 表
type SKU struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	ProductID int64     `gorm:"column:product_id"`
	Spec      string    `gorm:"column:spec"`
	Price     int64     `gorm:"column:price"`
	Status    SKUStatus `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (SKU) TableName() string {
	return "skus"
}
