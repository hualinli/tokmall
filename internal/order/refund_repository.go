package order

import (
	"context"
	"errors"
	"time"

	"github.com/lihualin/tokmall/internal/outbox"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GormRefundRepository 为退款命令提供独立事务边界。
//
// 并发控制策略（与下单/支付一致的两道防线）：
//  1. 所有退款命令都先对订单行 SELECT ... FOR UPDATE，把"检查状态"和"改状态"
//     放进同一把行锁里，消除先查后写的竞态窗口；
//  2. refunds 的 (order_id) WHERE status IN ('REQUESTED','SUCCEEDED') 部分唯一索引
//     是第二道防线，即使将来新增一条忘记加锁的代码路径，也不会产生两笔并行退款。
//
// 因此"申请退款 vs 发货"、"同意退款 vs 确认收货"、"两次同意"这些竞争，
// 最终都由订单行锁 + 状态条件更新仲裁成唯一合法终态。
func (r *GormRepository) RequestRefund(ctx context.Context, cmd RequestRefundCommand) (*refundRecord, error) {
	var result *refundRecord
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		var order Order
		err := db.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", cmd.OrderID, cmd.UserID).Take(&order).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrOrderNotFound
		}
		if err != nil {
			return err
		}

		// 重复申请（含网络重试与双击）返回同一笔退款单，不产生第二笔售后事实。
		if order.Status == StatusRefundRequested || order.Status == StatusRefunded {
			existing, err := findLatestRefund(db, order.ID)
			if err != nil {
				return err
			}
			if existing == nil {
				return errors.New("refund invariant violated: order in refund state has no refund document")
			}
			result = &refundRecord{Refund: *existing, Order: order, Replayed: true}
			return nil
		}
		if !refundableFrom(order.Status) {
			return ErrRefundNotAllowed
		}

		refund := Refund{
			RefundNo: cmd.RefundNo, OrderID: order.ID, UserID: order.UserID,
			MerchantID: order.MerchantID, Amount: order.TotalAmount, Reason: cmd.Reason,
			Status: RefundStatusRequested, RequestedAt: cmd.RequestedAt.UTC(),
		}
		claim := db.Clauses(clause.OnConflict{
			Columns:     []clause.Column{{Name: "order_id"}},
			TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "status IN ('REQUESTED', 'SUCCEEDED')"}}},
			DoNothing:   true,
		}).Create(&refund)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			existing, err := findLatestRefund(db, order.ID)
			if err != nil {
				return err
			}
			if existing == nil {
				return errors.New("refund invariant violated: conflicting refund claim cannot be loaded")
			}
			result = &refundRecord{Refund: *existing, Order: order, Replayed: true}
			return nil
		}

		at := cmd.RequestedAt.UTC()
		from := order.Status
		updated := db.Model(&Order{}).
			Where("id = ? AND status = ?", order.ID, string(from)).
			Updates(map[string]any{
				"status": string(StatusRefundRequested), "refund_from_status": string(from), "updated_at": at,
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrOrderStateConflict
		}
		order.Status = StatusRefundRequested
		order.RefundFromStatus = &from
		order.UpdatedAt = at
		result = &refundRecord{Refund: refund, Order: order}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *GormRepository) DecideRefund(ctx context.Context, cmd DecideRefundCommand) (*refundRecord, error) {
	var result *refundRecord
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		var order Order
		err := db.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND merchant_id = ?", cmd.OrderID, cmd.MerchantID).Take(&order).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrOrderNotFound
		}
		if err != nil {
			return err
		}

		// 已退款订单上的重复"同意"是重放；重复"拒绝"与对已裁决退款的再次裁决都是冲突。
		if order.Status == StatusRefunded {
			if !cmd.Approve {
				return ErrRefundAlreadyDecided
			}
			decided, err := findRefundByStatus(db, order.ID, RefundStatusSucceeded)
			if err != nil {
				return err
			}
			result = &refundRecord{Refund: *decided, Order: order, Replayed: true}
			return nil
		}
		if order.Status != StatusRefundRequested || order.RefundFromStatus == nil {
			return ErrRefundNotFound
		}
		pending, err := findRefundByStatus(db, order.ID, RefundStatusRequested)
		if err != nil {
			return err
		}
		if cmd.Approve {
			return approveRefund(db, order, *pending, cmd, &result)
		}
		return rejectRefund(db, order, *pending, cmd, &result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func approveRefund(db *gorm.DB, order Order, pending Refund, cmd DecideRefundCommand, result **refundRecord) error {
	at := cmd.DecidedAt.UTC()
	remark := nullableRemark(cmd.Remark)
	decided := db.Model(&Refund{}).
		Where("id = ? AND status = ?", pending.ID, string(RefundStatusRequested)).
		Updates(map[string]any{
			"status": string(RefundStatusSucceeded), "decided_at": at, "decided_by": cmd.DecidedBy,
			"decision_remark": remark, "refunded_at": at, "updated_at": at,
		})
	if decided.Error != nil {
		return decided.Error
	}
	if decided.RowsAffected != 1 {
		return ErrRefundAlreadyDecided
	}

	updated := db.Model(&Order{}).
		Where("id = ? AND status = ?", order.ID, string(StatusRefundRequested)).
		Updates(map[string]any{"status": string(StatusRefunded), "refunded_at": at, "updated_at": at})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrOrderStateConflict
	}

	// 未发货退款才回补库存：货还在仓库里，库存重新可售。
	// 已发货退款不回补——货已出库，退货入库需要独立的质检与上架流程。
	if order.RefundFromStatus != nil && *order.RefundFromStatus == StatusPaid {
		if err := restoreStock(db, order, at); err != nil {
			return err
		}
	}

	// 退款成功同样走 Outbox：投影、通知、对账都靠这条事件驱动。
	// 与支付一样，事件与状态变更在同一事务里提交或回滚。
	item, err := findOrderItem(db, order.ID)
	if err != nil {
		return err
	}
	if err := outbox.EnqueueOrderRefunded(db, outbox.OrderRefundedPayload{
		OrderID: order.ID, OrderNo: order.OrderNo, RefundNo: pending.RefundNo,
		UserID: order.UserID, MerchantID: order.MerchantID,
		Amount: int64(pending.Amount), Units: item.Quantity,
		RestoreStock:   order.RefundFromStatus != nil && *order.RefundFromStatus == StatusPaid,
		RefundedAt:     at,
		SourceAuthorID: order.SourceAuthorID, SourceVideoID: order.SourceVideoID,
	}); err != nil {
		return err
	}

	order.Status = StatusRefunded
	order.RefundedAt = &at
	order.UpdatedAt = at
	pending.Status = RefundStatusSucceeded
	pending.DecidedAt = &at
	pending.DecidedBy = &cmd.DecidedBy
	pending.DecisionRemark = remark
	pending.RefundedAt = &at
	pending.UpdatedAt = at
	*result = &refundRecord{Refund: pending, Order: order}
	return nil
}

func rejectRefund(db *gorm.DB, order Order, pending Refund, cmd DecideRefundCommand, result **refundRecord) error {
	at := cmd.DecidedAt.UTC()
	remark := nullableRemark(cmd.Remark)
	decided := db.Model(&Refund{}).
		Where("id = ? AND status = ?", pending.ID, string(RefundStatusRequested)).
		Updates(map[string]any{
			"status": string(RefundStatusRejected), "decided_at": at, "decided_by": cmd.DecidedBy,
			"decision_remark": remark, "updated_at": at,
		})
	if decided.Error != nil {
		return decided.Error
	}
	if decided.RowsAffected != 1 {
		return ErrRefundAlreadyDecided
	}

	// 拒绝后订单退回申请前状态：PAID 可继续发货，FULFILLING 可继续收货。
	restored := *order.RefundFromStatus
	updated := db.Model(&Order{}).
		Where("id = ? AND status = ?", order.ID, string(StatusRefundRequested)).
		Updates(map[string]any{
			"status": string(restored), "refund_from_status": nil, "updated_at": at,
		})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrOrderStateConflict
	}

	order.Status = restored
	order.RefundFromStatus = nil
	order.UpdatedAt = at
	pending.Status = RefundStatusRejected
	pending.DecidedAt = &at
	pending.DecidedBy = &cmd.DecidedBy
	pending.DecisionRemark = remark
	pending.UpdatedAt = at
	*result = &refundRecord{Refund: pending, Order: order}
	return nil
}

// restoreStock 回补库存并写流水。流水业务键 (sku_id, ORDER_REFUNDED, order_no)
// 唯一：如果同一订单试图回补两次，唯一约束会命中，此时必须整体回滚，
// 而不是默默再扣一次或再补一次库存。
func restoreStock(db *gorm.DB, order Order, at time.Time) error {
	item, err := findOrderItem(db, order.ID)
	if err != nil {
		return err
	}
	stock := db.Model(&inventoryRow{}).Where("sku_id = ?", item.SKUID).
		Updates(map[string]any{
			"quantity": gorm.Expr("quantity + ?", item.Quantity),
			"version":  gorm.Expr("version + 1"), "updated_at": at,
		})
	if stock.Error != nil {
		return stock.Error
	}
	if stock.RowsAffected != 1 {
		return errors.New("inventory invariant violated: order sku has no inventory row")
	}
	ledger := inventoryLedger{
		SKUID: item.SKUID, OrderID: order.ID, Change: int64(item.Quantity),
		Reason: "ORDER_REFUNDED", BizID: order.OrderNo,
	}
	inserted := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&ledger)
	if inserted.Error != nil {
		return translateError(inserted.Error)
	}
	if inserted.RowsAffected == 0 {
		return errors.New("inventory invariant violated: refund stock already restored for order")
	}
	return nil
}

func nullableRemark(remark string) *string {
	if remark == "" {
		return nil
	}
	return &remark
}

func findLatestRefund(db *gorm.DB, orderID int64) (*Refund, error) {
	var refund Refund
	err := db.Where("order_id = ?", orderID).Order("id DESC").Take(&refund).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &refund, nil
}

func findRefundByStatus(db *gorm.DB, orderID int64, status RefundStatus) (*Refund, error) {
	var refund Refund
	err := db.Where("order_id = ? AND status = ?", orderID, string(status)).Order("id DESC").Take(&refund).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRefundNotFound
	}
	if err != nil {
		return nil, err
	}
	return &refund, nil
}

// GetRefund 买家按 user_id、商家按 merchant_id 归属查询；查询他人订单一律 ErrOrderNotFound，
// 不泄露订单是否存在（与订单详情同一语义）。返回该订单最新一笔退款单。
func (r *GormRepository) GetRefund(ctx context.Context, actorID, orderID int64, merchant bool) (*refundRecord, error) {
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
	refund, err := findLatestRefund(r.db.WithContext(ctx), order.ID)
	if err != nil {
		return nil, err
	}
	if refund == nil {
		return nil, ErrRefundNotFound
	}
	return &refundRecord{Refund: *refund, Order: order}, nil
}
