package product

import "time"

// ProductStatus 商品状态。service 负责用户输入和状态迁移，数据库 CHECK
// 负责兜底所有写入口（API、Worker、运维脚本）的枚举不变量。
type ProductStatus string

const (
	ProductStatusOnSale  ProductStatus = "ON_SALE"
	ProductStatusOffSale ProductStatus = "OFF_SALE"
	ProductStatusDeleted ProductStatus = "DELETED"
)

// SKUStatus SKU 状态。数据库同样保留枚举 CHECK 作为最后一道防线。
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
	CoverKey    string        `gorm:"column:cover_key"`
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
