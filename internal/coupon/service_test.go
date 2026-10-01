package coupon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRepository 只实现领取路径需要的部分：把"券行锁 + 计数"的语义用内存模拟，
// 用来验证 service 层的规则（状态、有效期、门槛、限领）。
type fakeRepository struct {
	mu          sync.Mutex
	coupons     map[int64]*Coupon
	userCoupons map[string]int // key: couponID:userID → 已领数量
	nextID      int64
}

func newFakeRepository(coupons ...*Coupon) *fakeRepository {
	repo := &fakeRepository{coupons: map[int64]*Coupon{}, userCoupons: map[string]int{}}
	for _, c := range coupons {
		repo.coupons[c.ID] = c
	}
	return repo
}

func (f *fakeRepository) CreateCoupon(_ context.Context, c *Coupon) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	c.ID = f.nextID
	f.coupons[c.ID] = c
	return nil
}

func (f *fakeRepository) ListCoupons(context.Context, int64, int) ([]Coupon, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Coupon, 0, len(f.coupons))
	for _, c := range f.coupons {
		out = append(out, *c)
	}
	return out, nil
}

func (f *fakeRepository) ListAvailable(_ context.Context, userID int64, _ int, _ time.Time) ([]userCouponRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]userCouponRecord, 0, len(f.coupons))
	for _, c := range f.coupons {
		out = append(out, userCouponRecord{
			Coupon:       *c,
			ClaimedCount: int64(f.userCoupons[claimKey(c.ID, userID)]),
		})
	}
	return out, nil
}

func (f *fakeRepository) Claim(_ context.Context, cmd ClaimCommand) (*claimRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.coupons[cmd.CouponID]
	if !ok {
		return nil, ErrCouponNotFound
	}
	if c.Status != StatusActive {
		return nil, ErrCouponNotClaimable
	}
	if cmd.ClaimedAt.Before(c.ValidFrom) || !cmd.ClaimedAt.Before(c.ValidTo) {
		return nil, ErrCouponExpired
	}
	if c.TotalQuantity > 0 && c.IssuedQuantity >= c.TotalQuantity {
		return nil, ErrSoldOut
	}
	key := claimKey(cmd.CouponID, cmd.UserID)
	if f.userCoupons[key] >= int(c.PerUserLimit) {
		return nil, ErrAlreadyClaimed
	}
	c.IssuedQuantity++
	f.userCoupons[key]++
	f.nextID++
	return &claimRecord{
		Coupon: *c,
		UserCoupon: UserCoupon{
			ID: f.nextID, CouponID: c.ID, UserID: cmd.UserID,
			Status: UserCouponUnused, ClaimedAt: cmd.ClaimedAt,
		},
	}, nil
}

func (f *fakeRepository) ListUserCoupons(context.Context, int64, UserCouponStatus, int) ([]userCouponRecord, error) {
	return nil, nil
}

func (f *fakeRepository) GetUserCoupon(context.Context, int64, int64) (*userCouponRecord, error) {
	return nil, ErrUserCouponNotFound
}

func claimKey(couponID, userID int64) string {
	return fmt.Sprintf("%d:%d", couponID, userID)
}

func activeCoupon(id int64, total int64, perUser int32) *Coupon {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return &Coupon{
		ID: id, MerchantID: 1, Title: "满 100 减 20",
		ThresholdAmount: 10000, DiscountAmount: 2000,
		TotalQuantity: total, PerUserLimit: perUser,
		Status: StatusActive, ValidFrom: now.Add(-time.Hour), ValidTo: now.Add(time.Hour),
	}
}

func newTestService(repo Repository) *Service {
	svc := NewService(repo)
	svc.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	return svc
}

func TestCreateCouponValidatesInput(t *testing.T) {
	svc := newTestService(newFakeRepository())
	valid := CreateCouponParams{
		MerchantID: 1, Title: "满 100 减 20", ThresholdAmount: 10000, DiscountAmount: 2000,
		TotalQuantity: 100, PerUserLimit: 1,
		ValidFrom: time.Now(), ValidTo: time.Now().Add(time.Hour),
	}
	cases := map[string]func(*CreateCouponParams){
		"no merchant":             func(p *CreateCouponParams) { p.MerchantID = 0 },
		"blank title":             func(p *CreateCouponParams) { p.Title = "  " },
		"zero threshold":          func(p *CreateCouponParams) { p.ThresholdAmount = 0 },
		"zero discount":           func(p *CreateCouponParams) { p.DiscountAmount = 0 },
		"discount over threshold": func(p *CreateCouponParams) { p.DiscountAmount = 20000 },
		"negative quantity":       func(p *CreateCouponParams) { p.TotalQuantity = -1 },
		"zero per user":           func(p *CreateCouponParams) { p.PerUserLimit = 0 },
		"inverted window":         func(p *CreateCouponParams) { p.ValidTo = p.ValidFrom.Add(-time.Minute) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			params := valid
			mutate(&params)
			_, err := svc.CreateCoupon(context.Background(), params)
			require.ErrorIs(t, err, ErrInvalidParam)
		})
	}

	view, err := svc.CreateCoupon(context.Background(), valid)
	require.NoError(t, err)
	require.Equal(t, int64(100), view.Remaining)
	require.Equal(t, StatusActive, view.Status)
}

func TestClaimEnforcesRules(t *testing.T) {
	coupon := activeCoupon(1, 2, 1)
	repo := newFakeRepository(coupon)
	svc := newTestService(repo)

	first, err := svc.Claim(context.Background(), 100, 1)
	require.NoError(t, err)
	require.Equal(t, UserCouponUnused, first.Status)
	require.True(t, first.Usable)

	// 同一用户再领 → 达到每人限领
	_, err = svc.Claim(context.Background(), 100, 1)
	require.ErrorIs(t, err, ErrAlreadyClaimed)

	// 另一个用户领走最后一张 → 售罄
	_, err = svc.Claim(context.Background(), 101, 1)
	require.NoError(t, err)
	_, err = svc.Claim(context.Background(), 102, 1)
	require.ErrorIs(t, err, ErrSoldOut)

	// 发行量不会被超发
	require.Equal(t, int64(2), repo.coupons[1].IssuedQuantity)
}

func TestClaimRejectsExpiredAndDisabled(t *testing.T) {
	expired := activeCoupon(1, 10, 1)
	expired.ValidTo = time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	disabled := activeCoupon(2, 10, 1)
	disabled.Status = StatusDisabled

	svc := newTestService(newFakeRepository(expired, disabled))
	_, err := svc.Claim(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrCouponExpired)
	_, err = svc.Claim(context.Background(), 1, 2)
	require.ErrorIs(t, err, ErrCouponNotClaimable)
}

// TestDiscountForIsTheSingleRule：门槛按整单金额、状态与有效期都校验。
func TestDiscountForIsTheSingleRule(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c := Coupon{ID: 1, ThresholdAmount: 10000, DiscountAmount: 2000, ValidFrom: now.Add(-time.Hour), ValidTo: now.Add(time.Hour)}
	unused := UserCoupon{Status: UserCouponUnused}

	discount, err := DiscountFor(unused, c, 10000, now)
	require.NoError(t, err)
	require.Equal(t, int64(2000), discount, "刚好达到门槛即可用")

	discount, err = DiscountFor(unused, c, 50000, now)
	require.NoError(t, err)
	require.Equal(t, int64(2000), discount, "满减是固定面额，不随金额放大")

	_, err = DiscountFor(unused, c, 9999, now)
	require.ErrorIs(t, err, ErrThresholdNotMet)

	_, err = DiscountFor(UserCoupon{Status: UserCouponUsed}, c, 20000, now)
	require.ErrorIs(t, err, ErrUserCouponUsed)

	expiredCoupon := c
	expiredCoupon.ValidTo = now.Add(-time.Minute)
	_, err = DiscountFor(unused, expiredCoupon, 20000, now)
	require.ErrorIs(t, err, ErrCouponExpired)
}

// TestListAvailableMarksClaimedByMe：服务端算好"我已领过"，
// 避免前端重复实现规则（DomTok 的领取次数由客户端传，服务端不校验）。
func TestListAvailableMarksClaimedByMe(t *testing.T) {
	repo := newFakeRepository(activeCoupon(1, 10, 1))
	svc := newTestService(repo)

	views, err := svc.ListAvailable(context.Background(), 100, 10)
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.False(t, views[0].ClaimedByMe)

	_, err = svc.Claim(context.Background(), 100, 1)
	require.NoError(t, err)

	views, err = svc.ListAvailable(context.Background(), 100, 10)
	require.NoError(t, err)
	require.True(t, views[0].ClaimedByMe, "已达限领的券要标记出来")
}

// TestExpiryIsComputedAtReadTime：过期是时间的函数，读取时判定即可，
// 不需要定时任务刷状态（但状态一旦核销就必须落库）。
func TestExpiryIsComputedAtReadTime(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c := Coupon{ID: 1, Title: "t", ThresholdAmount: 100, DiscountAmount: 10, ValidFrom: now.Add(-2 * time.Hour), ValidTo: now.Add(-time.Hour)}
	view := toUserCouponView(UserCoupon{ID: 5, CouponID: 1, Status: UserCouponUnused}, c, now)
	require.Equal(t, UserCouponExpired, view.Status)
	require.False(t, view.Usable)

	// 未过期时 usable 为真
	c.ValidTo = now.Add(time.Hour)
	view = toUserCouponView(UserCoupon{ID: 5, CouponID: 1, Status: UserCouponUnused}, c, now)
	require.Equal(t, UserCouponUnused, view.Status)
	require.True(t, view.Usable)
}
