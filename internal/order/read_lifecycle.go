package order

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxOrderPageSize = 50

type OrderView struct {
	OrderID      int64      `json:"order_id"`
	OrderNo      string     `json:"order_no"`
	UserID       int64      `json:"user_id"`
	MerchantID   int64      `json:"merchant_id"`
	Status       Status     `json:"status"`
	TotalAmount  Amount     `json:"total_amount"`
	Source       *Source    `json:"source,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
	PaidAt       *time.Time `json:"paid_at,omitempty"`
	FulfillingAt *time.Time `json:"fulfilling_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	CanceledAt   *time.Time `json:"canceled_at,omitempty"`
	ExpiredAt    *time.Time `json:"expired_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	Item         ItemView   `json:"item"`
}

type OrderPage struct {
	Items        []OrderView `json:"items"`
	NextBeforeID *int64      `json:"next_before_id,omitempty"`
}

type TransitionResult struct {
	OrderID  int64  `json:"order_id"`
	OrderNo  string `json:"order_no"`
	Status   Status `json:"status"`
	Replayed bool   `json:"replayed"`
}

type orderRecord struct {
	Order Order
	Item  OrderItem
}

type transitionRecord struct {
	Order    Order
	Replayed bool
}

func (s *Service) Get(ctx context.Context, actorID, orderID int64, merchant bool) (*OrderView, error) {
	if actorID <= 0 || orderID <= 0 {
		return nil, ErrInvalidParam
	}
	record, err := s.repo.Get(ctx, actorID, orderID, merchant)
	if err != nil {
		return nil, err
	}
	view := mapOrderView(*record)
	return &view, nil
}

func (s *Service) List(ctx context.Context, actorID, beforeID int64, limit int, merchant bool) (*OrderPage, error) {
	if actorID <= 0 || beforeID < 0 || limit <= 0 || limit > maxOrderPageSize {
		return nil, ErrInvalidParam
	}
	records, err := s.repo.List(ctx, actorID, beforeID, limit+1, merchant)
	if err != nil {
		return nil, err
	}
	page := &OrderPage{Items: make([]OrderView, 0, min(len(records), limit))}
	for _, record := range records[:min(len(records), limit)] {
		page.Items = append(page.Items, mapOrderView(record))
	}
	if len(records) > limit {
		last := page.Items[len(page.Items)-1].OrderID
		page.NextBeforeID = &last
	}
	return page, nil
}

func (s *Service) Fulfill(ctx context.Context, merchantID, orderID int64) (*TransitionResult, error) {
	return s.transition(ctx, merchantID, orderID, true, StatusFulfilling)
}

func (s *Service) Complete(ctx context.Context, buyerID, orderID int64) (*TransitionResult, error) {
	return s.transition(ctx, buyerID, orderID, false, StatusCompleted)
}

func (s *Service) transition(ctx context.Context, actorID, orderID int64, merchant bool, to Status) (*TransitionResult, error) {
	if actorID <= 0 || orderID <= 0 {
		return nil, ErrInvalidParam
	}
	record, err := s.repo.Transition(ctx, actorID, orderID, merchant, to, s.now())
	if err != nil {
		return nil, err
	}
	return &TransitionResult{OrderID: record.Order.ID, OrderNo: record.Order.OrderNo,
		Status: record.Order.Status, Replayed: record.Replayed}, nil
}

func mapOrderView(record orderRecord) OrderView {
	o, item := record.Order, record.Item
	view := OrderView{
		OrderID: o.ID, OrderNo: o.OrderNo, UserID: o.UserID, MerchantID: o.MerchantID,
		Status: o.Status, TotalAmount: o.TotalAmount, ExpiresAt: o.ExpiresAt,
		PaidAt: o.PaidAt, FulfillingAt: o.FulfillingAt, CompletedAt: o.CompletedAt,
		CanceledAt: o.CanceledAt, ExpiredAt: o.ExpiredAt, CreatedAt: o.CreatedAt,
		Item: ItemView{ProductID: item.ProductID, SKUID: item.SKUID,
			MerchantID: item.MerchantIDSnapshot, ProductTitle: item.ProductTitleSnapshot,
			SKUSpec: item.SKUSpecSnapshot, UnitPrice: item.UnitPrice,
			Quantity: item.Quantity, LineAmount: item.LineAmount},
	}
	if o.SourceVideoID != nil && o.SourceAuthorID != nil && o.SourceProductID != nil {
		view.Source = &Source{VideoID: *o.SourceVideoID, AuthorID: *o.SourceAuthorID, ProductID: *o.SourceProductID}
	}
	return view
}

func (r *GormRepository) Get(ctx context.Context, actorID, orderID int64, merchant bool) (*orderRecord, error) {
	owner := "user_id"
	if merchant {
		owner = "merchant_id"
	}
	var order Order
	err := r.db.WithContext(ctx).Where("id = ? AND "+owner+" = ?", orderID, actorID).Take(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	item, err := findOrderItem(r.db.WithContext(ctx), order.ID)
	if err != nil {
		return nil, err
	}
	return &orderRecord{Order: order, Item: *item}, nil
}

func (r *GormRepository) List(ctx context.Context, actorID, beforeID int64, limit int, merchant bool) ([]orderRecord, error) {
	owner := "user_id"
	if merchant {
		owner = "merchant_id"
	}
	query := r.db.WithContext(ctx).Where(owner+" = ?", actorID)
	if beforeID > 0 {
		query = query.Where("id < ?", beforeID)
	}
	var orders []Order
	if err := query.Order("id DESC").Limit(limit).Find(&orders).Error; err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return []orderRecord{}, nil
	}
	ids := make([]int64, len(orders))
	for i, order := range orders {
		ids[i] = order.ID
	}
	var items []OrderItem
	if err := r.db.WithContext(ctx).Where("order_id IN ?", ids).Find(&items).Error; err != nil {
		return nil, err
	}
	byOrder := make(map[int64]OrderItem, len(items))
	for _, item := range items {
		byOrder[item.OrderID] = item
	}
	result := make([]orderRecord, 0, len(orders))
	for _, order := range orders {
		item, ok := byOrder[order.ID]
		if !ok {
			return nil, errors.New("order invariant violated: existing order has no item")
		}
		result = append(result, orderRecord{Order: order, Item: item})
	}
	return result, nil
}

func (r *GormRepository) Transition(ctx context.Context, actorID, orderID int64, merchant bool, to Status, at time.Time) (*transitionRecord, error) {
	if (merchant && to != StatusFulfilling) || (!merchant && to != StatusCompleted) {
		return nil, ErrInvalidParam
	}
	owner := "user_id"
	column := "completed_at"
	if merchant {
		owner = "merchant_id"
		column = "fulfilling_at"
	}
	var result *transitionRecord
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		var order Order
		err := db.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND "+owner+" = ?", orderID, actorID).Take(&order).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrOrderNotFound
		}
		if err != nil {
			return err
		}
		if order.Status == to {
			result = &transitionRecord{Order: order, Replayed: true}
			return nil
		}
		if !canOrderTransition(order.Status, to) {
			return ErrOrderStateConflict
		}
		at = at.UTC()
		updated := db.Model(&Order{}).Where("id = ? AND status = ?", order.ID, string(order.Status)).
			Updates(map[string]any{"status": string(to), column: at, "updated_at": at})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrOrderStateConflict
		}
		order.Status = to
		order.UpdatedAt = at
		if merchant {
			order.FulfillingAt = &at
		} else {
			order.CompletedAt = &at
		}
		result = &transitionRecord{Order: order}
		return nil
	})
	return result, err
}
