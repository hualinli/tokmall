package order

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/lihualin/tokmall/internal/outbox"
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
	requestedAt := cmd.RequestedAt.UTC()
	respondBy := cmd.RespondBy.UTC()
	if !respondBy.After(requestedAt) {
		return nil, errors.New("refund respond_by must be after requested_at")
	}
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
			Status: RefundStatusRequested, RequestedAt: requestedAt, RespondBy: respondBy,
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

		at := requestedAt
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
		// 商家路径在这里把命令收成裁决事实。超时自动同意会自己构造一份，
		// 两条路都进 approveRefund，避免以后各写一套同意逻辑。
		decision := merchantDecision(cmd)
		if cmd.Approve {
			return approveRefund(db, order, *pending, decision, &result)
		}
		return rejectRefund(db, order, *pending, decision, cmd.MinAutoCompleteAt, &result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ApproveOverdueRefund 在一个事务里处理一笔到期仍未裁决的退款：先锁订单行，
// 再把 TIMEOUT 裁决交给 approveRefund。没有到期行时返回 false。
//
// 只锁 orders（FOR UPDATE OF o SKIP LOCKED），不在这条 SELECT 里锁 refunds。
// DecideRefund 也是先锁订单再改退款单；如果这里先锁退款单，worker 和商家会互相等待。
// 商家正握着订单锁时，这一笔被跳过，等下一轮再看。
func (r *GormRepository) ApproveOverdueRefund(ctx context.Context, now time.Time) (bool, error) {
	now = now.UTC()
	var approved *refundRecord
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		var locked struct {
			ID int64 `gorm:"column:id"`
		}
		err := db.Raw(`
			SELECT o.id
			FROM orders AS o
			JOIN refunds AS rf ON rf.order_id = o.id
			WHERE o.status = ? AND rf.status = ? AND rf.respond_by <= ?
			ORDER BY rf.respond_by, rf.id
			LIMIT 1
			FOR UPDATE OF o SKIP LOCKED`,
			string(StatusRefundRequested), string(RefundStatusRequested), now,
		).Scan(&locked).Error
		if err != nil {
			return err
		}
		if locked.ID == 0 {
			return nil
		}
		var order Order
		err = db.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", locked.ID).Take(&order).Error
		if err != nil {
			return err
		}
		pending, err := findRefundByStatus(db, order.ID, RefundStatusRequested)
		if errors.Is(err, ErrRefundNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// FOR UPDATE OF o 只锁订单。READ COMMITTED 下，订单行若在本语句快照之后被提交过，
		// PG 会取最新版本并重查 WHERE；JOIN 进来的 refunds 不会刷新，仍用快照里的旧行。
		// 查询条件只负责挑出候选。锁内再用订单和当前待处理退款单重新判断，
		// 否则商家拒绝旧单、买家立刻再申请后，可能把还没到期的新退款单自动同意。
		if order.Status != StatusRefundRequested || pending.RespondBy.After(now) {
			return nil
		}
		var result *refundRecord
		if err := approveRefund(db, order, *pending, timeoutDecision(now), &result); err != nil {
			return err
		}
		approved = result
		return nil
	})
	if err != nil {
		return false, err
	}
	if approved == nil {
		return false, nil
	}
	slog.InfoContext(ctx, "auto approved overdue refund",
		"order_id", approved.Order.ID, "refund_no", approved.Refund.RefundNo)
	return true, nil
}

// decisionSource 区分裁决是商家做出的，还是处理时限到期后系统做出的。
type decisionSource string

const (
	decisionSourceMerchant decisionSource = "MERCHANT"
	decisionSourceTimeout  decisionSource = "TIMEOUT"
)

// refundDecision 是写入退款单的裁决事实：时间、裁决人、来源、备注。
// 商家同意/拒绝与超时自动同意各自构造，approveRefund 不再接收整份 DecideRefundCommand
// （那份命令还带着订单、是否同意、自动确认顺延，都不是退款单上的字段）。
type refundDecision struct {
	At        time.Time
	DecidedBy *int64
	Source    decisionSource
	Remark    string
}

func merchantDecision(cmd DecideRefundCommand) refundDecision {
	decidedBy := cmd.DecidedBy
	return refundDecision{
		At:        cmd.DecidedAt,
		DecidedBy: &decidedBy,
		Source:    decisionSourceMerchant,
		Remark:    cmd.Remark,
	}
}

func timeoutDecision(at time.Time) refundDecision {
	return refundDecision{At: at.UTC(), Source: decisionSourceTimeout}
}

// writeColumns 生成退款单裁决列。时间、备注和来源都从裁决事实本身取，
// 避免调用方另传一份、和 d.At / d.Remark 对不上。
// 来源决定 decided_by：商家必须有人，超时必须为空，而且超时只能同意。
func (d refundDecision) writeColumns(status RefundStatus) (map[string]any, error) {
	at := d.At.UTC()
	cols := map[string]any{
		"status": string(status), "decided_at": at, "decision_remark": nullableRemark(d.Remark),
		"decision_source": string(d.Source), "updated_at": at,
	}
	switch d.Source {
	case decisionSourceMerchant:
		if d.DecidedBy == nil || *d.DecidedBy <= 0 {
			return nil, errors.New("merchant refund decision requires decided_by")
		}
		cols["decided_by"] = *d.DecidedBy
	case decisionSourceTimeout:
		if status != RefundStatusSucceeded {
			return nil, errors.New("timeout refund decision can only approve")
		}
		if d.DecidedBy != nil {
			return nil, errors.New("timeout refund decision must leave decided_by empty")
		}
		// map 更新里的 nil 会写成 NULL。跳过零值只发生在用 struct 更新时。
		cols["decided_by"] = nil
	default:
		return nil, errors.New("refund decision source is required")
	}
	return cols, nil
}

func (d refundDecision) applyTo(refund *Refund, status RefundStatus) {
	at := d.At.UTC()
	refund.Status = status
	refund.DecidedAt = &at
	refund.DecisionRemark = nullableRemark(d.Remark)
	source := string(d.Source)
	refund.DecisionSource = &source
	refund.UpdatedAt = at
	if d.DecidedBy == nil {
		refund.DecidedBy = nil
		return
	}
	id := *d.DecidedBy
	refund.DecidedBy = &id
}

func approveRefund(db *gorm.DB, order Order, pending Refund, decision refundDecision, result **refundRecord) error {
	at := decision.At.UTC()
	cols, err := decision.writeColumns(RefundStatusSucceeded)
	if err != nil {
		return err
	}
	cols["refunded_at"] = at
	decided := db.Model(&Refund{}).
		Where("id = ? AND status = ?", pending.ID, string(RefundStatusRequested)).
		Updates(cols)
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

	// 退券：全额退款成功后把券退回可再用状态（前提是券仍在有效期内）。
	// 这是 DomTok 完全缺失的一环——它的退款既不写金额也不处理券，
	// 券一旦核销就永久消失，用户退款后拿不回优惠。
	if err := restoreUserCoupon(db, order, at); err != nil {
		return err
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
	decision.applyTo(&pending, RefundStatusSucceeded)
	pending.RefundedAt = &at
	*result = &refundRecord{Refund: pending, Order: order}
	return nil
}

func rejectRefund(db *gorm.DB, order Order, pending Refund, decision refundDecision, minAutoCompleteAt time.Time, result **refundRecord) error {
	at := decision.At.UTC()
	cols, err := decision.writeColumns(RefundStatusRejected)
	if err != nil {
		return err
	}
	decided := db.Model(&Refund{}).
		Where("id = ? AND status = ?", pending.ID, string(RefundStatusRequested)).
		Updates(cols)
	if decided.Error != nil {
		return decided.Error
	}
	if decided.RowsAffected != 1 {
		return ErrRefundAlreadyDecided
	}

	// 拒绝后订单退回申请前状态：PAID 可继续发货，FULFILLING 可继续收货。
	restored := *order.RefundFromStatus
	changes := map[string]any{"status": string(restored), "refund_from_status": nil, "updated_at": at}
	var resumed *time.Time
	if restored == StatusFulfilling {
		if order.AutoCompleteAt == nil {
			return errors.New("order invariant violated: fulfilled order has no auto complete deadline")
		}
		// 退款申请期间截止时间可能已经过去；顺延到至少 MinAutoCompleteAt，
		// 买家仍有时间确认收货或再次申请，不会在拒绝的下一刻被自动确认。
		deadline := *order.AutoCompleteAt
		if minimum := minAutoCompleteAt.UTC(); deadline.Before(minimum) {
			deadline = minimum
		}
		changes["auto_complete_at"] = deadline
		resumed = &deadline
	}
	updated := db.Model(&Order{}).
		Where("id = ? AND status = ?", order.ID, string(StatusRefundRequested)).
		Updates(changes)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrOrderStateConflict
	}

	if resumed != nil {
		order.AutoCompleteAt = resumed
	}
	order.Status = restored
	order.RefundFromStatus = nil
	order.UpdatedAt = at
	decision.applyTo(&pending, RefundStatusRejected)
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

// restoreUserCoupon 退回退款成功或未支付关单占用的券：
//   - 券仍在有效期内 → UNUSED（清空 used_at/order_id，用户可以再次使用）；
//   - 已过期 → EXPIRED（退也没用，但状态要如实反映，便于对账）。
//
// 退券与订单终态同事务提交；数据库错误回滚整次操作，允许安全重试。
func restoreUserCoupon(db *gorm.DB, order Order, at time.Time) error {
	if order.UserCouponID == nil {
		return nil
	}
	var row struct {
		ID      int64     `gorm:"column:id"`
		ValidTo time.Time `gorm:"column:valid_to"`
		Status  string    `gorm:"column:status"`
		OrderID *int64    `gorm:"column:order_id"`
	}
	err := db.Raw(`SELECT uc.id, c.valid_to, uc.status, uc.order_id
		FROM user_coupons uc JOIN coupons c ON c.id = uc.coupon_id
		WHERE uc.id = ? FOR UPDATE OF uc`, *order.UserCouponID).Scan(&row).Error
	if err != nil {
		return err
	}
	if row.ID == 0 {
		// 券记录被删（人工运维）：退款仍然有效，只记日志。
		slog.Warn("refund cannot restore coupon: user coupon missing",
			"order_id", order.ID, "user_coupon_id", *order.UserCouponID)
		return nil
	}
	if row.Status != "USED" || row.OrderID == nil || *row.OrderID != order.ID {
		// 券不是被本单用掉的（数据不一致或已退回）：不动它，留给对账暴露。
		slog.Warn("refund found coupon not used by this order",
			"order_id", order.ID, "user_coupon_id", row.ID, "coupon_status", row.Status)
		return nil
	}

	status := "UNUSED"
	if !at.Before(row.ValidTo) {
		status = "EXPIRED"
	}
	updated := db.Model(&userCouponRow{}).
		Where("id = ? AND status = ? AND order_id = ?", row.ID, "USED", order.ID).
		Updates(map[string]any{
			"status": status, "used_at": nil, "order_id": nil, "updated_at": at,
		})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return errors.New("refund invariant violated: coupon restore raced with another update")
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
		// 没有退款单是正常状态（不是错误），调用方按 nil 判断。
		return nil, nil //nolint:nilnil // 见上方注释：不存在不是错误
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
