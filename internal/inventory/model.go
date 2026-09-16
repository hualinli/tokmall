package inventory

import "time"

// Inventory 对应 inventory 表（sku_id 一比一）
type Inventory struct {
	SkuID     int64     `gorm:"column:sku_id;primaryKey"`
	Quantity  int64     `gorm:"column:quantity"`
	Version   int64     `gorm:"column:version"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (Inventory) TableName() string {
	return "inventory"
}

// Reason 流水原因，类型化防拼错（到 GORM 边界显式转 string）
type Reason string

const (
	ReasonAdjust        Reason = "ADJUST"         // 商家人工调整
	ReasonOrderCreated  Reason = "ORDER_CREATED"  // 下单扣减（第三阶段使用）
	ReasonOrderCanceled Reason = "ORDER_CANCELED" // 取消回补
	ReasonOrderExpired  Reason = "ORDER_EXPIRED"  // 超时关单回补
)

// InventoryTransaction 对应 inventory_transactions 表。
// 只增不改：流水是对账的事实依据，任何情况下不允许 UPDATE/DELETE。
type InventoryTransaction struct {
	ID      int64  `gorm:"column:id;primaryKey"`
	SkuID   int64  `gorm:"column:sku_id"`
	OrderID *int64 `gorm:"column:order_id"`
	Change  int64  `gorm:"column:change"` // 正数=入库，负数=扣减
	Reason  Reason `gorm:"column:reason"`
	// BizID 是调用方提供的业务幂等键。唯一范围由 (sku_id, reason, biz_id)
	// 共同定义；不能只依赖 biz_id 全局唯一，否则不同 SKU 复用客户端 key
	// 会被错误地判定为同一笔库存操作。
	BizID     string    `gorm:"column:biz_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (InventoryTransaction) TableName() string {
	return "inventory_transactions"
}
