package coupon

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Service 是优惠券域的领域服务。规则（能不能领、能不能用、减多少）
// 全部收敛在这里，repository 只负责带锁的读写。
type Service struct {
	repo Repository
	now  func() time.Time
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: time.Now}
}

// CreateCoupon 商家创建券模板。校验全部在服务端，不接受客户端传"已发放数量"这类字段。
func (s *Service) CreateCoupon(ctx context.Context, p CreateCouponParams) (*CouponView, error) {
	title := strings.TrimSpace(p.Title)
	switch {
	case p.MerchantID <= 0:
		return nil, ErrInvalidParam
	case title == "" || utf8.RuneCountInString(title) > maxTitleLength:
		return nil, ErrInvalidParam
	case p.ThresholdAmount <= 0 || p.DiscountAmount <= 0:
		return nil, ErrInvalidParam
	case p.DiscountAmount > p.ThresholdAmount:
		// 满 100 减 200 会算出负数订单；这里挡住，数据库 CHECK 再兜一层。
		return nil, ErrInvalidParam
	case p.TotalQuantity < 0 || p.TotalQuantity > maxTotalQuantity:
		return nil, ErrInvalidParam
	case p.PerUserLimit <= 0 || p.PerUserLimit > maxPerUserLimit:
		return nil, ErrInvalidParam
	case !p.ValidTo.After(p.ValidFrom):
		return nil, ErrInvalidParam
	}
	c := &Coupon{
		MerchantID: p.MerchantID, Title: title,
		ThresholdAmount: p.ThresholdAmount, DiscountAmount: p.DiscountAmount,
		TotalQuantity: p.TotalQuantity, PerUserLimit: p.PerUserLimit,
		Status: StatusActive, ValidFrom: p.ValidFrom.UTC(), ValidTo: p.ValidTo.UTC(),
	}
	if err := s.repo.CreateCoupon(ctx, c); err != nil {
		return nil, err
	}
	view := toCouponView(*c)
	return &view, nil
}

func (s *Service) ListMerchantCoupons(ctx context.Context, merchantID int64, limit int) ([]CouponView, error) {
	if merchantID <= 0 {
		return nil, ErrInvalidParam
	}
	coupons, err := s.repo.ListCoupons(ctx, merchantID, normalizeLimit(limit))
	if err != nil {
		return nil, err
	}
	views := make([]CouponView, 0, len(coupons))
	for _, c := range coupons {
		views = append(views, toCouponView(c))
	}
	return views, nil
}

// ListAvailable 返回用户可领取的券（含"我已领过"标记）。
func (s *Service) ListAvailable(ctx context.Context, userID int64, limit int) ([]CouponView, error) {
	if userID <= 0 {
		return nil, ErrInvalidParam
	}
	records, err := s.repo.ListAvailable(ctx, userID, normalizeLimit(limit), s.now())
	if err != nil {
		return nil, err
	}
	views := make([]CouponView, 0, len(records))
	for _, record := range records {
		view := toCouponView(record.Coupon)
		view.ClaimedByMe = record.ClaimedCount >= int64(record.Coupon.PerUserLimit)
		views = append(views, view)
	}
	return views, nil
}

// Claim 领券。重复领取返回 ErrAlreadyClaimed（前端显示"已领取"），
// 而不是静默成功——静默成功会让用户以为又领到一张。
func (s *Service) Claim(ctx context.Context, userID, couponID int64) (*UserCouponView, error) {
	if userID <= 0 || couponID <= 0 {
		return nil, ErrInvalidParam
	}
	record, err := s.repo.Claim(ctx, ClaimCommand{CouponID: couponID, UserID: userID, ClaimedAt: s.now()})
	if err != nil {
		return nil, err
	}
	view := toUserCouponView(record.UserCoupon, record.Coupon, s.now())
	return &view, nil
}

// ListMyCoupons 我的券。status 为空时返回全部；过期的券在返回时被标记为 EXPIRED
// （不写库：过期是时间的函数，没必要用定时任务刷状态；真正需要落库的是核销）。
func (s *Service) ListMyCoupons(ctx context.Context, userID int64, status string, limit int) ([]UserCouponView, error) {
	if userID <= 0 {
		return nil, ErrInvalidParam
	}
	parsed, err := parseUserCouponStatus(status)
	if err != nil {
		return nil, err
	}
	records, err := s.repo.ListUserCoupons(ctx, userID, parsed, normalizeLimit(limit))
	if err != nil {
		return nil, err
	}
	now := s.now()
	views := make([]UserCouponView, 0, len(records))
	for _, record := range records {
		views = append(views, toUserCouponView(record.UserCoupon, record.Coupon, now))
	}
	return views, nil
}

// GetUserCoupon 查询单张持券（下单前校验、前端展示用）。
func (s *Service) GetUserCoupon(ctx context.Context, userID, userCouponID int64) (*UserCouponView, error) {
	if userID <= 0 || userCouponID <= 0 {
		return nil, ErrInvalidParam
	}
	record, err := s.repo.GetUserCoupon(ctx, userID, userCouponID)
	if err != nil {
		return nil, err
	}
	view := toUserCouponView(record.UserCoupon, record.Coupon, s.now())
	return &view, nil
}

// DiscountFor 是"这张券能减多少"的唯一实现：订单侧与视图侧都调用它，
// 避免两处各写一套门槛判断（DomTok 的券逻辑散在算价、创建、领取三处，
// 最后金额字段语义全乱了）。
//
// 规则：状态 UNUSED、在有效期内、订单商品金额 >= 门槛 → 返回券面额；
// 否则返回领域错误。注意门槛比较的是**整单商品金额**（本项目 v1 是单 SKU 订单，
// 所以等于该行金额；将来支持多商品时这里已经是整单语义）。
func DiscountFor(uc UserCoupon, c Coupon, orderAmount int64, now time.Time) (int64, error) {
	if uc.Status != UserCouponUnused {
		return 0, ErrUserCouponUsed
	}
	if now.Before(c.ValidFrom) || !now.Before(c.ValidTo) {
		return 0, ErrCouponExpired
	}
	if orderAmount < c.ThresholdAmount {
		return 0, ErrThresholdNotMet
	}
	discount := c.DiscountAmount
	if discount > orderAmount {
		discount = orderAmount
	}
	return discount, nil
}

func parseUserCouponStatus(raw string) (UserCouponStatus, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case string(UserCouponUnused):
		return UserCouponUnused, nil
	case string(UserCouponUsed):
		return UserCouponUsed, nil
	case string(UserCouponExpired):
		return UserCouponExpired, nil
	default:
		return "", ErrInvalidParam
	}
}

func normalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return 20
	case limit > 100:
		return 100
	default:
		return limit
	}
}

func toCouponView(c Coupon) CouponView {
	return CouponView{
		CouponID: c.ID, MerchantID: c.MerchantID, Title: c.Title,
		ThresholdAmount: c.ThresholdAmount, DiscountAmount: c.DiscountAmount,
		TotalQuantity: c.TotalQuantity, IssuedQuantity: c.IssuedQuantity,
		Remaining: c.Remaining(), PerUserLimit: c.PerUserLimit,
		Status: c.Status, ValidFrom: c.ValidFrom, ValidTo: c.ValidTo,
	}
}

func toUserCouponView(uc UserCoupon, c Coupon, now time.Time) UserCouponView {
	status := uc.Status
	if status == UserCouponUnused && (now.Before(c.ValidFrom) || !now.Before(c.ValidTo)) {
		// 过期是时间的函数：读取时判定，不需要定时任务刷库。
		status = UserCouponExpired
	}
	return UserCouponView{
		UserCouponID: uc.ID, CouponID: c.ID, Title: c.Title,
		ThresholdAmount: c.ThresholdAmount, DiscountAmount: c.DiscountAmount,
		Status: status, ValidFrom: c.ValidFrom, ValidTo: c.ValidTo,
		ClaimedAt: uc.ClaimedAt, UsedAt: uc.UsedAt, OrderID: uc.OrderID,
		Usable: status == UserCouponUnused && !now.Before(c.ValidFrom) && now.Before(c.ValidTo),
	}
}

var _ = errors.Is
