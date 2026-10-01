package coupon

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type GormRepository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *GormRepository { return &GormRepository{db: db} }

func (r *GormRepository) CreateCoupon(ctx context.Context, c *Coupon) error {
	return r.db.WithContext(ctx).Create(c).Error
}

func (r *GormRepository) ListCoupons(ctx context.Context, merchantID int64, limit int) ([]Coupon, error) {
	var coupons []Coupon
	err := r.db.WithContext(ctx).Where("merchant_id = ?", merchantID).
		Order("id DESC").Limit(limit).Find(&coupons).Error
	if err != nil {
		return nil, err
	}
	return coupons, nil
}

// ListAvailable 返回用户**可以领取**的券：ACTIVE + 在有效期内 + 还有余量。
//
// 它同时算出 claimed_by_me：前端不需要自己判断"我领过没有"，
// 服务端把规则算好返回，避免前端与后端各写一套判断（DomTok 的教训之一是
// 领取次数由客户端传，服务端完全不校验）。
func (r *GormRepository) ListAvailable(ctx context.Context, userID int64, limit int, now time.Time) ([]userCouponRecord, error) {
	var coupons []Coupon
	err := r.db.WithContext(ctx).
		Where("status = ? AND valid_from <= ? AND valid_to > ?", string(StatusActive), now.UTC(), now.UTC()).
		Where("total_quantity = 0 OR issued_quantity < total_quantity").
		Order("valid_to ASC, id ASC").Limit(limit).Find(&coupons).Error
	if err != nil {
		return nil, err
	}
	if len(coupons) == 0 {
		return []userCouponRecord{}, nil
	}

	ids := make([]int64, 0, len(coupons))
	for i := range coupons {
		ids = append(ids, coupons[i].ID)
	}
	type claimCount struct {
		CouponID int64
		Count    int64
	}
	var counts []claimCount
	if err := r.db.WithContext(ctx).Model(&UserCoupon{}).
		Select("coupon_id, count(*) AS count").
		Where("user_id = ? AND coupon_id IN ?", userID, ids).
		Group("coupon_id").Scan(&counts).Error; err != nil {
		return nil, err
	}
	claimed := make(map[int64]int64, len(counts))
	for _, row := range counts {
		claimed[row.CouponID] = row.Count
	}

	records := make([]userCouponRecord, 0, len(coupons))
	for i := range coupons {
		records = append(records, userCouponRecord{
			Coupon:       coupons[i],
			ClaimedCount: claimed[coupons[i].ID],
		})
	}
	return records, nil
}

// Claim 是并发正确性的核心：整段在一个事务里，第一步就锁住券行。
//
// 为什么锁券行而不是锁用户行：发行量是**券级别**的资源，所有领取者竞争同一行；
// 拿到这把锁之后，"检查余量 → 扣减 → 检查每人限领 → 发券"四步之间不可能有
// 其他领取者插入，因此既不会超发，也不会超领。条件更新（issued < total）
// 是第二道防线：即使将来有人绕过锁，数据库也不会让发行量越过上限。
func (r *GormRepository) Claim(ctx context.Context, cmd ClaimCommand) (*claimRecord, error) {
	var result *claimRecord
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var c Coupon
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", cmd.CouponID).Take(&c).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrCouponNotFound
		}
		if err != nil {
			return err
		}

		at := cmd.ClaimedAt.UTC()
		if c.Status != StatusActive {
			return ErrCouponNotClaimable
		}
		if at.Before(c.ValidFrom) || !at.Before(c.ValidTo) {
			// 领取截止：DomTok 只在创建时比较时间，领取路径从不校验，
			// 结果是"过期券还能领"。这里领取时必须校验。
			return ErrCouponExpired
		}

		claimed := tx.Model(&Coupon{}).
			Where("id = ? AND (total_quantity = 0 OR issued_quantity < total_quantity)", c.ID).
			Updates(map[string]any{
				"issued_quantity": gorm.Expr("issued_quantity + 1"),
				"updated_at":      at,
			})
		if claimed.Error != nil {
			return claimed.Error
		}
		if claimed.RowsAffected != 1 {
			return ErrSoldOut
		}

		var mine int64
		if err := tx.Model(&UserCoupon{}).
			Where("coupon_id = ? AND user_id = ?", c.ID, cmd.UserID).Count(&mine).Error; err != nil {
			return err
		}
		if mine >= int64(c.PerUserLimit) {
			// 返回错误 → 整个事务回滚 → 上面加的发行量也不会留下。
			return ErrAlreadyClaimed
		}

		userCoupon := UserCoupon{
			CouponID: c.ID, UserID: cmd.UserID,
			Status: UserCouponUnused, ClaimedAt: at, UpdatedAt: at,
		}
		if err := tx.Create(&userCoupon).Error; err != nil {
			return err
		}
		c.IssuedQuantity++
		result = &claimRecord{Coupon: c, UserCoupon: userCoupon}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *GormRepository) ListUserCoupons(ctx context.Context, userID int64, status UserCouponStatus, limit int) ([]userCouponRecord, error) {
	return r.listUserCoupons(ctx, userID, status, 0, limit)
}

func (r *GormRepository) GetUserCoupon(ctx context.Context, userID, userCouponID int64) (*userCouponRecord, error) {
	records, err := r.listUserCoupons(ctx, userID, "", userCouponID, 1)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		// 券不属于当前用户与券不存在返回同一错误，不泄露存在性。
		return nil, ErrUserCouponNotFound
	}
	return &records[0], nil
}

// listUserCoupons 是"我的券"的统一查询：持券信息 join 券模板。
// userCouponID > 0 时只查那一张（用于下单前校验归属）。
func (r *GormRepository) listUserCoupons(ctx context.Context, userID int64, status UserCouponStatus, userCouponID int64, limit int) ([]userCouponRecord, error) {
	query := r.db.WithContext(ctx).Table("user_coupons uc").
		Select("uc.id, uc.coupon_id, uc.user_id, uc.status, uc.claimed_at, uc.used_at, uc.order_id, uc.updated_at, "+
			"c.id AS c_id, c.merchant_id, c.title, c.threshold_amount, c.discount_amount, c.total_quantity, "+
			"c.issued_quantity, c.per_user_limit, c.status AS c_status, c.valid_from, c.valid_to, "+
			"c.created_at AS c_created_at, c.updated_at AS c_updated_at").
		Joins("JOIN coupons c ON c.id = uc.coupon_id").
		Where("uc.user_id = ?", userID)
	if status != "" {
		query = query.Where("uc.status = ?", string(status))
	}
	if userCouponID > 0 {
		query = query.Where("uc.id = ?", userCouponID)
	}
	var rows []struct {
		UserCoupon
		CouponIDAlias   int64     `gorm:"column:c_id"`
		MerchantID      int64     `gorm:"column:merchant_id"`
		Title           string    `gorm:"column:title"`
		ThresholdAmount int64     `gorm:"column:threshold_amount"`
		DiscountAmount  int64     `gorm:"column:discount_amount"`
		TotalQuantity   int64     `gorm:"column:total_quantity"`
		IssuedQuantity  int64     `gorm:"column:issued_quantity"`
		PerUserLimit    int32     `gorm:"column:per_user_limit"`
		CouponStatus    Status    `gorm:"column:c_status"`
		ValidFrom       time.Time `gorm:"column:valid_from"`
		ValidTo         time.Time `gorm:"column:valid_to"`
		CouponCreatedAt time.Time `gorm:"column:c_created_at"`
		CouponUpdatedAt time.Time `gorm:"column:c_updated_at"`
	}
	if limit <= 0 {
		limit = 100
	}
	if err := query.Order("uc.id DESC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]userCouponRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, userCouponRecord{
			UserCoupon: row.UserCoupon,
			Coupon: Coupon{
				ID: row.CouponIDAlias, MerchantID: row.MerchantID, Title: row.Title,
				ThresholdAmount: row.ThresholdAmount, DiscountAmount: row.DiscountAmount,
				TotalQuantity: row.TotalQuantity, IssuedQuantity: row.IssuedQuantity,
				PerUserLimit: row.PerUserLimit, Status: row.CouponStatus,
				ValidFrom: row.ValidFrom, ValidTo: row.ValidTo,
				CreatedAt: row.CouponCreatedAt, UpdatedAt: row.CouponUpdatedAt,
			},
		})
	}
	return records, nil
}
