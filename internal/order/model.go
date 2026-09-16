package order

import "time"

// Amount 表示最小货币单位的整数金额。Commerce v1 只处理人民币分，
// 不用 float，避免序列化、计算和数据库之间出现舍入差异。
type Amount int64

type Status string

const (
	StatusPendingPayment Status = "PENDING_PAYMENT"
	StatusPaid           Status = "PAID"
	StatusFulfilling     Status = "FULFILLING"
	StatusCompleted      Status = "COMPLETED"
	StatusCanceled       Status = "CANCELED"
	StatusExpired        Status = "EXPIRED"
)

type PaymentStatus string

const (
	PaymentStatusPending PaymentStatus = "PENDING"
	PaymentStatusSuccess PaymentStatus = "SUCCESS"
	PaymentStatusFailed  PaymentStatus = "FAILED"
)

type VideoProductStatus string

const (
	VideoProductStatusActive   VideoProductStatus = "ACTIVE"
	VideoProductStatusDisabled VideoProductStatus = "DISABLED"
)

// Order 对应 orders 表。Source* 是下单时归因快照 ID，不声明 GORM 关联，
// 避免内容域对象后续变化影响订单事实。
type Order struct {
	ID                 int64      `gorm:"column:id;primaryKey"`
	OrderNo            string     `gorm:"column:order_no"`
	UserID             int64      `gorm:"column:user_id"`
	MerchantID         int64      `gorm:"column:merchant_id"`
	Status             Status     `gorm:"column:status"`
	TotalAmount        Amount     `gorm:"column:total_amount"`
	IdempotencyKey     string     `gorm:"column:idempotency_key"`
	RequestFingerprint string     `gorm:"column:request_fingerprint"`
	SourceVideoID      *int64     `gorm:"column:source_video_id"`
	SourceAuthorID     *int64     `gorm:"column:source_author_id"`
	SourceProductID    *int64     `gorm:"column:source_product_id"`
	ExpiresAt          time.Time  `gorm:"column:expires_at"`
	PaidAt             *time.Time `gorm:"column:paid_at"`
	CanceledAt         *time.Time `gorm:"column:canceled_at"`
	ExpiredAt          *time.Time `gorm:"column:expired_at"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

func (Order) TableName() string { return "orders" }

// OrderItem 保存成交时的商品事实。ProductID/SKUID 用于追踪，展示与金额
// 必须使用 snapshot 字段，不能回查商品当前标题、规格或价格。
type OrderItem struct {
	ID                   int64     `gorm:"column:id;primaryKey"`
	OrderID              int64     `gorm:"column:order_id"`
	ProductID            int64     `gorm:"column:product_id"`
	SKUID                int64     `gorm:"column:sku_id"`
	MerchantIDSnapshot   int64     `gorm:"column:merchant_id_snapshot"`
	ProductTitleSnapshot string    `gorm:"column:product_title_snapshot"`
	SKUSpecSnapshot      string    `gorm:"column:sku_spec_snapshot"`
	UnitPrice            Amount    `gorm:"column:unit_price"`
	Quantity             int32     `gorm:"column:quantity"`
	LineAmount           Amount    `gorm:"column:line_amount"`
	CreatedAt            time.Time `gorm:"column:created_at"`
}

func (OrderItem) TableName() string { return "order_items" }

// Payment 是支付尝试而非订单的一对一扩展。一笔订单可以有多次失败/重试，
// payment_no 和第三方交易号分别承担内部与外部幂等标识。
type Payment struct {
	ID              int64         `gorm:"column:id;primaryKey"`
	PaymentNo       string        `gorm:"column:payment_no"`
	OrderID         int64         `gorm:"column:order_id"`
	Amount          Amount        `gorm:"column:amount"`
	Status          PaymentStatus `gorm:"column:status"`
	ProviderTradeNo *string       `gorm:"column:provider_trade_no"`
	FailureCode     *string       `gorm:"column:failure_code"`
	PaidAt          *time.Time    `gorm:"column:paid_at"`
	CreatedAt       time.Time     `gorm:"column:created_at"`
	UpdatedAt       time.Time     `gorm:"column:updated_at"`
}

func (Payment) TableName() string { return "payments" }

// VideoProduct 是内容域与商品域的推广关系。author_id 是受控冗余字段，
// 写入时必须由后续 service 校验等于视频作者，而不能相信客户端。
type VideoProduct struct {
	ID        int64              `gorm:"column:id;primaryKey"`
	VideoID   int64              `gorm:"column:video_id"`
	AuthorID  int64              `gorm:"column:author_id"`
	ProductID int64              `gorm:"column:product_id"`
	Status    VideoProductStatus `gorm:"column:status"`
	SortOrder int32              `gorm:"column:sort_order"`
	CreatedAt time.Time          `gorm:"column:created_at"`
	UpdatedAt time.Time          `gorm:"column:updated_at"`
}

func (VideoProduct) TableName() string { return "video_products" }

// SellableSKU 是订单模块自己的只读投影。订单事务必须把当前商品事实复制成
// 快照，不能把 product 模块的可变 model 暴露到订单领域。
type SellableSKU struct {
	SKUID        int64  `gorm:"column:sku_id"`
	ProductID    int64  `gorm:"column:product_id"`
	MerchantID   int64  `gorm:"column:merchant_id"`
	ProductTitle string `gorm:"column:product_title"`
	SKUSpec      string `gorm:"column:sku_spec"`
	UnitPrice    Amount `gorm:"column:unit_price"`
}
