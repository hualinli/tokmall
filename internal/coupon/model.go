// Package coupon 实现优惠券/促销域（v1：满减券）。
//
// 业务规则见 docs/business.md，事务约束见
// docs/architecture.md 的优惠券一节：发行量、每人限领、领取截止、
// 券状态机、整数分金额、整单门槛、退款退券、对账覆盖——缺一条就会出现
// "券可以无限领、无限用、过期还能领"那类问题。
package coupon

import (
	"errors"
	"time"
)

var (
	ErrInvalidParam       = errors.New("invalid param")
	ErrCouponNotFound     = errors.New("coupon not found")
	ErrCouponNotClaimable = errors.New("coupon is not claimable")
	ErrSoldOut            = errors.New("coupon is sold out")
	ErrAlreadyClaimed     = errors.New("per-user claim limit reached")
	ErrUserCouponNotFound = errors.New("user coupon not found")
	ErrUserCouponUsed     = errors.New("user coupon already used")
	ErrThresholdNotMet    = errors.New("order amount does not meet the coupon threshold")
	ErrCouponExpired      = errors.New("coupon expired")
)

const (
	maxTitleLength = 100
	// maxTotalQuantity 是单次发行的上界：防止误填一个巨大的数字让 CHECK 失去意义。
	maxTotalQuantity = 1_000_000
	maxPerUserLimit  = 100
)

type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
)

type UserCouponStatus string

const (
	UserCouponUnused  UserCouponStatus = "UNUSED"
	UserCouponUsed    UserCouponStatus = "USED"
	UserCouponExpired UserCouponStatus = "EXPIRED"
)

// Coupon 对应 coupons 表（券模板）。
type Coupon struct {
	ID              int64     `gorm:"column:id;primaryKey"`
	MerchantID      int64     `gorm:"column:merchant_id"`
	Title           string    `gorm:"column:title"`
	ThresholdAmount int64     `gorm:"column:threshold_amount"`
	DiscountAmount  int64     `gorm:"column:discount_amount"`
	TotalQuantity   int64     `gorm:"column:total_quantity"`
	IssuedQuantity  int64     `gorm:"column:issued_quantity"`
	PerUserLimit    int32     `gorm:"column:per_user_limit"`
	Status          Status    `gorm:"column:status"`
	ValidFrom       time.Time `gorm:"column:valid_from"`
	ValidTo         time.Time `gorm:"column:valid_to"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

func (Coupon) TableName() string { return "coupons" }

// Remaining 返回剩余可领数量；不限量时返回 -1。
func (c *Coupon) Remaining() int64 {
	if c.TotalQuantity == 0 {
		return -1
	}
	remaining := c.TotalQuantity - c.IssuedQuantity
	if remaining < 0 {
		return 0
	}
	return remaining
}

// UserCoupon 对应 user_coupons 表（用户持有的券）。
type UserCoupon struct {
	ID        int64            `gorm:"column:id;primaryKey"`
	CouponID  int64            `gorm:"column:coupon_id"`
	UserID    int64            `gorm:"column:user_id"`
	Status    UserCouponStatus `gorm:"column:status"`
	ClaimedAt time.Time        `gorm:"column:claimed_at"`
	UsedAt    *time.Time       `gorm:"column:used_at"`
	OrderID   *int64           `gorm:"column:order_id"`
	UpdatedAt time.Time        `gorm:"column:updated_at"`
}

func (UserCoupon) TableName() string { return "user_coupons" }
