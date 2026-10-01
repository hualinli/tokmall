package coupon

import (
	"context"
	"time"
)

// Service 的领域命令与视图。Repository 定义在消费侧，GormRepository 隐式满足。

type CreateCouponParams struct {
	MerchantID      int64
	Title           string
	ThresholdAmount int64
	DiscountAmount  int64
	TotalQuantity   int64
	PerUserLimit    int32
	ValidFrom       time.Time
	ValidTo         time.Time
}

// CouponView 是券模板的对外视图（model 不出 service）。
type CouponView struct {
	CouponID        int64     `json:"coupon_id"`
	MerchantID      int64     `json:"merchant_id"`
	Title           string    `json:"title"`
	ThresholdAmount int64     `json:"threshold_amount"`
	DiscountAmount  int64     `json:"discount_amount"`
	TotalQuantity   int64     `json:"total_quantity"`
	IssuedQuantity  int64     `json:"issued_quantity"`
	Remaining       int64     `json:"remaining"`
	PerUserLimit    int32     `json:"per_user_limit"`
	Status          Status    `json:"status"`
	ValidFrom       time.Time `json:"valid_from"`
	ValidTo         time.Time `json:"valid_to"`
	// ClaimedByMe 只在"可领取列表"里填充：让前端能直接显示"已领取"。
	ClaimedByMe bool `json:"claimed_by_me"`
}

// UserCouponView 是用户持券的对外视图。
type UserCouponView struct {
	UserCouponID    int64            `json:"user_coupon_id"`
	CouponID        int64            `json:"coupon_id"`
	Title           string           `json:"title"`
	ThresholdAmount int64            `json:"threshold_amount"`
	DiscountAmount  int64            `json:"discount_amount"`
	Status          UserCouponStatus `json:"status"`
	ValidFrom       time.Time        `json:"valid_from"`
	ValidTo         time.Time        `json:"valid_to"`
	ClaimedAt       time.Time        `json:"claimed_at"`
	UsedAt          *time.Time       `json:"used_at,omitempty"`
	OrderID         *int64           `json:"order_id,omitempty"`
	// Usable 由服务端算好：状态未使用 + 在有效期内。前端不需要重复判断规则。
	Usable bool `json:"usable"`
}

// ClaimCommand 是领券命令。RefundCommand 之外的所有写操作都在仓储层的事务里完成。
type ClaimCommand struct {
	CouponID  int64
	UserID    int64
	ClaimedAt time.Time
}

type claimRecord struct {
	Coupon     Coupon
	UserCoupon UserCoupon
	Replayed   bool
}

type userCouponRecord struct {
	UserCoupon UserCoupon
	Coupon     Coupon
	// ClaimedCount 只在 ListAvailable 里填充：当前用户对这张券已领几张。
	// 显式字段而不是塞进 UserCoupon.ID——后者可读性差且容易在测试里漏填。
	ClaimedCount int64
}

// Repository 由 service 定义、消费。
type Repository interface {
	CreateCoupon(ctx context.Context, c *Coupon) error
	ListCoupons(ctx context.Context, merchantID int64, limit int) ([]Coupon, error)
	ListAvailable(ctx context.Context, userID int64, limit int, now time.Time) ([]userCouponRecord, error)
	Claim(ctx context.Context, cmd ClaimCommand) (*claimRecord, error)
	ListUserCoupons(ctx context.Context, userID int64, status UserCouponStatus, limit int) ([]userCouponRecord, error)
	GetUserCoupon(ctx context.Context, userID, userCouponID int64) (*userCouponRecord, error)
}
