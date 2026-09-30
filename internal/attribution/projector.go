package attribution

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lihualin/tokmall/internal/outbox"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Projector 是作者日销投影的消费者：把 OrderPaid / OrderRefunded 事件
// 折叠成按 (author_id, stat_date) 聚合的读模型。
//
// 为什么要有这张表（而不是继续实时聚合 orders）：
//   - 实时聚合随订单量增长会越来越慢，且每次查询都要扫订单表；
//   - 事件驱动投影让"作者看板"变成一次主键查询；
//   - 代价是最终一致：投影可能落后于事实，因此必须有一个对账任务
//     比较投影与实时聚合（见 internal/reconcile）。
//
// 幂等性由 Consumer 保证：回执（consumed_events）与本函数的写入在同一事务，
// 重复投递不会把同一笔订单累加两次。
type Projector struct {
	db *gorm.DB
}

func NewProjector(db *gorm.DB) *Projector { return &Projector{db: db} }

func (p *Projector) EventTypes() []string {
	return []string{outbox.EventTypeOrderPaid, outbox.EventTypeOrderRefunded}
}

func (p *Projector) Handle(ctx context.Context, tx *gorm.DB, event outbox.Event) error {
	switch event.EventType {
	case outbox.EventTypeOrderPaid:
		var payload outbox.OrderPaidPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode OrderPaid payload: %w", err)
		}
		if payload.SourceAuthorID == nil {
			// 非带货订单（没有来源作者）不进作者投影：它不属于任何作者的业绩。
			return nil
		}
		return p.applyPaid(tx, *payload.SourceAuthorID, payload)
	case outbox.EventTypeOrderRefunded:
		var payload outbox.OrderRefundedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode OrderRefunded payload: %w", err)
		}
		if payload.SourceAuthorID == nil {
			return nil
		}
		return p.applyRefunded(tx, *payload.SourceAuthorID, payload)
	default:
		return nil
	}
}

// applyPaid 累加当日成交。ON CONFLICT DO UPDATE 是必要的：
// 同一作者同一天会有很多订单，投影必须做增量累加而不是覆盖。
func (p *Projector) applyPaid(tx *gorm.DB, authorID int64, payload outbox.OrderPaidPayload) error {
	row := AuthorSalesDaily{
		AuthorID:   authorID,
		StatDate:   statDate(payload.PaidAt),
		OrderCount: 1,
		UnitsSold:  int64(payload.Units),
		GMV:        payload.TotalAmount,
		UpdatedAt:  time.Now().UTC(),
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "author_id"}, {Name: "stat_date"}},
		DoUpdates: clause.Assignments(map[string]any{
			"order_count": gorm.Expr("author_sales_daily.order_count + ?", row.OrderCount),
			"units_sold":  gorm.Expr("author_sales_daily.units_sold + ?", row.UnitsSold),
			"gmv":         gorm.Expr("author_sales_daily.gmv + ?", row.GMV),
			"updated_at":  row.UpdatedAt,
		}),
	}).Create(&row).Error
}

// applyRefunded 记录退款。退款按退款发生日归集，而不是回冲原成交日：
// 对账口径是"每日净额"，把退款记在当天才能让每天的账都对得上。
func (p *Projector) applyRefunded(tx *gorm.DB, authorID int64, payload outbox.OrderRefundedPayload) error {
	row := AuthorSalesDaily{
		AuthorID:       authorID,
		StatDate:       statDate(payload.RefundedAt),
		RefundedOrders: 1,
		RefundedAmount: payload.Amount,
		UpdatedAt:      time.Now().UTC(),
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "author_id"}, {Name: "stat_date"}},
		DoUpdates: clause.Assignments(map[string]any{
			"refunded_orders": gorm.Expr("author_sales_daily.refunded_orders + ?", row.RefundedOrders),
			"refunded_amount": gorm.Expr("author_sales_daily.refunded_amount + ?", row.RefundedAmount),
			"updated_at":      row.UpdatedAt,
		}),
	}).Create(&row).Error
}

// AuthorSalesDaily 对应 author_sales_daily 表（事件驱动读模型）。
type AuthorSalesDaily struct {
	AuthorID       int64     `gorm:"column:author_id;primaryKey"`
	StatDate       time.Time `gorm:"column:stat_date;primaryKey;type:date"`
	OrderCount     int64     `gorm:"column:order_count"`
	UnitsSold      int64     `gorm:"column:units_sold"`
	GMV            int64     `gorm:"column:gmv"`
	RefundedOrders int64     `gorm:"column:refunded_orders"`
	RefundedAmount int64     `gorm:"column:refunded_amount"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (AuthorSalesDaily) TableName() string { return "author_sales_daily" }

func statDate(at time.Time) time.Time {
	utc := at.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// DailySales 读取投影（作者看板用）。它只读投影表，不触碰订单事实。
func (p *Projector) DailySales(ctx context.Context, authorID int64, from, to time.Time) ([]AuthorSalesDaily, error) {
	var rows []AuthorSalesDaily
	err := p.db.WithContext(ctx).
		Where("author_id = ? AND stat_date >= ? AND stat_date <= ?", authorID, statDate(from), statDate(to)).
		Order("stat_date DESC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
