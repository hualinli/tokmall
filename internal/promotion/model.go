package promotion

import "time"

type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
)

// VideoProduct 是 promotion 模块拥有的推广关系模型。order 模块即使读取
// 同一张表，也保留自己的 read model，避免模块间共享领域类型。
type VideoProduct struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	VideoID   int64     `gorm:"column:video_id"`
	AuthorID  int64     `gorm:"column:author_id"`
	ProductID int64     `gorm:"column:product_id"`
	Status    Status    `gorm:"column:status"`
	SortOrder int32     `gorm:"column:sort_order"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (VideoProduct) TableName() string { return "video_products" }

// ProductCard 是内容侧公开展示所需的最小只读投影。库存是交易域事实，
// 不进入该 DTO；封面 URL 在 3B Feed 批量装配时统一签发。
type ProductCard struct {
	ProductID     int64  `gorm:"column:product_id" json:"product_id"`
	Title         string `gorm:"column:title" json:"title"`
	MinPrice      int64  `gorm:"column:min_price" json:"min_price"`
	ProductStatus string `gorm:"column:product_status" json:"product_status"`
	SortOrder     int32  `gorm:"column:sort_order" json:"sort_order"`
}
