package attribution

import (
	"context"
	"errors"
)

var ErrInvalidAuthor = errors.New("invalid author")

// Sales preserves gross paid history and reports successful refunds separately. The source IDs are immutable order snapshots;
// current video-product relations are deliberately irrelevant to historical sales.
type Sales struct {
	AuthorID       int64   `json:"author_id"`
	OrderCount     int64   `json:"order_count"`
	BuyerCount     int64   `json:"buyer_count"`
	UnitsSold      int64   `json:"units_sold"`
	RefundedOrders int64   `json:"refunded_orders"`
	RefundedAmount int64   `json:"refunded_amount"`
	GMV            int64   `json:"gmv"`
	ByVideo        []Slice `json:"by_video"        gorm:"-"`
	ByProduct      []Slice `json:"by_product"      gorm:"-"`
}

type Slice struct {
	ID             int64 `json:"id"`
	OrderCount     int64 `json:"order_count"`
	BuyerCount     int64 `json:"buyer_count"`
	UnitsSold      int64 `json:"units_sold"`
	RefundedOrders int64 `json:"refunded_orders"`
	RefundedAmount int64 `json:"refunded_amount"`
	GMV            int64 `json:"gmv"`
}

type Repository interface {
	Sales(ctx context.Context, authorID int64) (*Sales, error)
}

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

func (s *Service) GetSales(ctx context.Context, authorID int64) (*Sales, error) {
	if authorID <= 0 {
		return nil, ErrInvalidAuthor
	}
	return s.repo.Sales(ctx, authorID)
}
