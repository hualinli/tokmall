package order

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/lihualin/tokmall/internal/coupon"
	"github.com/lihualin/tokmall/internal/outbox"
)

type GormRepository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *GormRepository { return &GormRepository{db: db} }

func (r *GormRepository) Create(ctx context.Context, cmd CreateCommand) (*createRecord, error) {
	var result *createRecord
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		// 同键请求先串行化，再查原订单；否则等待券锁的重试会先读到 USED。
		// 事务锁随提交/回滚释放。64 位哈希碰撞只会多串行化，不影响唯一约束。
		lockKey := fmt.Sprintf("tokmall:order:create:%d:%s", cmd.UserID, cmd.IdempotencyKey)
		if err := db.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", lockKey).Error; err != nil {
			return fmt.Errorf("lock order idempotency key: %w", err)
		}
		// 已完成订单的重放不应重新依赖商品当前状态：商品在首单后下架，
		// 客户端仍必须能用原 key 拿到原结果。默认 READ COMMITTED 下，
		// 等待锁后这次查询可见前一事务已提交的订单；唯一约束仍作为兜底。
		existing, err := findExistingOrder(db, cmd.UserID, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.RequestFingerprint != cmd.RequestFingerprint {
				return ErrIdempotencyConflict
			}
			existingItem, err := findOrderItem(db, existing.ID)
			if err != nil {
				return err
			}
			result = &createRecord{Order: *existing, Item: *existingItem, Replayed: true}
			return nil
		}

		sku, err := findSellableSKU(db, cmd.SKUID)
		if err != nil {
			return err
		}
		if cmd.Source != nil {
			if cmd.Source.ProductID != sku.ProductID {
				return ErrNotFound
			}
			if err := requireActiveSource(db, *cmd.Source); err != nil {
				return err
			}
		}
		lineAmount, ok := multiplyAmount(sku.UnitPrice, cmd.Quantity)
		if !ok {
			return ErrInvalidParam
		}

		// 用券：锁住持券行 → 校验归属/状态/有效期/门槛 → 算出优惠。
		// 锁在扣库存之前，且所有路径都用同一顺序（券 → 库存），避免死锁。
		discount := Amount(0)
		var userCouponID *int64
		if cmd.UserCouponID != nil {
			// 注意传的是"当前时间"而不是 cmd.ExpiresAt：后者是订单支付截止时间，
			// 用它判断券是否在有效期内会把窗口放大 30 分钟。
			applied, err := applyUserCoupon(db, cmd.UserID, sku.MerchantID, *cmd.UserCouponID, int64(lineAmount), time.Now())
			if err != nil {
				return err
			}
			discount = Amount(applied)
			userCouponID = cmd.UserCouponID
		}
		payable := lineAmount - discount
		if payable <= 0 {
			// 券面额把订单金额吃光会让 orders.total_amount 违反 CHECK；
			// 这里显式拒绝，语义是"优惠后必须仍有应付金额"。
			return ErrInvalidParam
		}

		order := Order{
			OrderNo: cmd.OrderNo, UserID: cmd.UserID, MerchantID: sku.MerchantID,
			Status: StatusPendingPayment, TotalAmount: payable,
			OriginalAmount: lineAmount, DiscountAmount: discount, UserCouponID: userCouponID,
			IdempotencyKey: cmd.IdempotencyKey, RequestFingerprint: cmd.RequestFingerprint,
			ExpiresAt: cmd.ExpiresAt,
		}
		if cmd.Source != nil {
			order.SourceVideoID = int64ptr(cmd.Source.VideoID)
			order.SourceAuthorID = int64ptr(cmd.Source.AuthorID)
			order.SourceProductID = int64ptr(cmd.Source.ProductID)
		}

		claim := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "idempotency_key"}},
			DoNothing: true,
		}).Create(&order)
		if claim.Error != nil {
			return translateError(claim.Error)
		}
		if claim.RowsAffected == 0 {
			existing, err := findExistingOrder(db, cmd.UserID, cmd.IdempotencyKey)
			if err != nil {
				return translateError(err)
			}
			if existing == nil {
				return errors.New("idempotency claim exists but order cannot be loaded")
			}
			if existing.RequestFingerprint != cmd.RequestFingerprint {
				return ErrIdempotencyConflict
			}
			existingItem, err := findOrderItem(db, existing.ID)
			if err != nil {
				return err
			}
			result = &createRecord{Order: *existing, Item: *existingItem, Replayed: true}
			return nil
		}

		ledger := inventoryLedger{
			SKUID: cmd.SKUID, OrderID: order.ID, Change: -int64(cmd.Quantity),
			Reason: "ORDER_CREATED", BizID: order.OrderNo,
		}
		if err := db.Create(&ledger).Error; err != nil {
			return translateError(err)
		}
		item := OrderItem{
			OrderID: order.ID, ProductID: sku.ProductID, SKUID: sku.SKUID,
			MerchantIDSnapshot: sku.MerchantID, ProductTitleSnapshot: sku.ProductTitle,
			SKUSpecSnapshot: sku.SKUSpec, UnitPrice: sku.UnitPrice,
			Quantity: cmd.Quantity, LineAmount: lineAmount,
		}
		if err := db.Create(&item).Error; err != nil {
			return translateError(err)
		}

		// 核销券：条件更新（只在 UNUSED 时生效）。锁已经保证不会并发，
		// 这里是第二道防线——RowsAffected != 1 说明券状态被别的路径改了，
		// 必须整体回滚，绝不能出现"订单用了券但券还是未使用"。
		if userCouponID != nil {
			used := db.Model(&userCouponRow{}).
				Where("id = ? AND user_id = ? AND status = ?", *userCouponID, cmd.UserID, "UNUSED").
				Updates(map[string]any{
					"status": "USED", "used_at": time.Now().UTC(),
					"order_id": order.ID, "updated_at": time.Now().UTC(),
				})
			if used.Error != nil {
				return used.Error
			}
			if used.RowsAffected != 1 {
				return ErrCouponConflict
			}
		}

		// 超时事件与订单同事务写入，available_at = expires_at：
		// 到点后中继才投递，消费者据此提前关单（轮询 Worker 仍是安全网）。
		// 放在这里而不是 service 层，是为了保证"订单存在 ⟺ 超时事件存在"。
		if err := outbox.EnqueueOrderTimeout(db, outbox.OrderTimeoutPayload{
			OrderID: order.ID, OrderNo: order.OrderNo, ExpiresAt: order.ExpiresAt,
		}); err != nil {
			return err
		}

		// 独立写入已准备好，再竞争热点库存行。扣减之后直接提交，
		// 不再持有库存锁等待明细、流水、券核销和 Outbox 的数据库往返。
		// 条件扣减失败会回滚整个事务，包括上面已插入的全部事实。
		stock := db.Model(&inventoryRow{}).
			Where("sku_id = ? AND quantity >= ?", cmd.SKUID, cmd.Quantity).
			Updates(map[string]any{
				"quantity":   gorm.Expr("quantity - ?", cmd.Quantity),
				"version":    gorm.Expr("version + 1"),
				"updated_at": time.Now(),
			})
		if stock.Error != nil {
			return stock.Error
		}
		if stock.RowsAffected == 0 {
			var count int64
			if err := db.Model(&inventoryRow{}).Where("sku_id = ?", cmd.SKUID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return errors.New("inventory invariant violated: existing sku has no inventory row")
			}
			return ErrInsufficientStock
		}

		result = &createRecord{Order: order, Item: item}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *GormRepository) Pay(ctx context.Context, cmd PayCommand) (*paymentRecord, error) {
	var result *paymentRecord
	// 支付事务顺带关单时，不能直接 return ErrOrderExpired——事务闭包返回
	// 错误会回滚，刚刚写下的关单与库存回补会被一起撤销。因此这里先在
	// 事务内提交副作用并置位标记，提交后再把"业务失败"返回给调用方。
	expiredDuringPay := false
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

		existing, err := findPaymentByIdentity(db, cmd.PaymentNo, cmd.ProviderTradeNo)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameSuccessfulPayment(*existing, order.ID, cmd) {
				return ErrIdempotencyConflict
			}
			result = &paymentRecord{Order: order, Payment: *existing, Replayed: true}
			return nil
		}
		if !canOrderTransition(order.Status, StatusPaid) {
			return ErrOrderStateConflict
		}
		// 过期判定必须在支付事务里自己做，不能只依赖超时关单 Worker：
		// Worker 有吞吐上限、可能被关闭，而且它用 SKIP LOCKED 扫描——
		// 正在被本事务锁住的行会被跳过，于是"支付被拒 + Worker 也跳过"
		// 会让订单停在 PENDING_PAYMENT。这里顺手把订单关掉（lazy expiry），
		// 支付路径自己给出确定终态，Worker 只作为兜底扫描。
		if !cmd.PaidAt.UTC().Before(order.ExpiresAt) {
			if err := expireLockedOrder(db, order, cmd.PaidAt.UTC()); err != nil {
				return err
			}
			expiredDuringPay = true
			return nil
		}
		if order.TotalAmount != cmd.Amount {
			return ErrPaymentAmountMismatch
		}

		providerTradeNo := cmd.ProviderTradeNo
		paidAt := cmd.PaidAt.UTC()
		payment := Payment{
			PaymentNo: cmd.PaymentNo, OrderID: order.ID, Amount: order.TotalAmount,
			Status: PaymentStatusSuccess, ProviderTradeNo: &providerTradeNo, PaidAt: &paidAt,
		}
		if err := db.Create(&payment).Error; err != nil {
			return translatePaymentWriteError(err)
		}
		updated := db.Model(&Order{}).
			Where("id = ? AND status = ? AND expires_at > ?", order.ID, string(StatusPendingPayment), paidAt).
			Updates(map[string]any{
				"status": string(StatusPaid), "paid_at": paidAt, "updated_at": paidAt,
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrOrderStateConflict
		}
		// Transactional Outbox：事件与订单状态在同一事务提交。
		// 提交成功但进程随后崩溃时，事件仍在表里，中继会补投；
		// 反过来，事务回滚时事件也一起消失——不会出现"订单没支付成功但下游已收到通知"。
		item, err := findOrderItem(db, order.ID)
		if err != nil {
			return err
		}
		if err := outbox.EnqueueOrderPaid(db, outbox.OrderPaidPayload{
			OrderID: order.ID, OrderNo: order.OrderNo, UserID: order.UserID,
			MerchantID: order.MerchantID, TotalAmount: int64(order.TotalAmount),
			Units: item.Quantity, PaidAt: paidAt,
			SourceAuthorID: order.SourceAuthorID, SourceVideoID: order.SourceVideoID,
			SourceProductID: order.SourceProductID,
		}); err != nil {
			return err
		}

		order.Status = StatusPaid
		order.PaidAt = &paidAt
		order.UpdatedAt = paidAt
		result = &paymentRecord{Order: order, Payment: payment}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if expiredDuringPay {
		// 关单副作用已提交，这里只负责把业务失败告诉调用方。
		return nil, ErrOrderExpired
	}
	return result, nil
}

func (r *GormRepository) Cancel(ctx context.Context, userID, orderID int64, canceledAt time.Time) (*cancelRecord, error) {
	var result *cancelRecord
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		var order Order
		err := db.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", orderID, userID).Take(&order).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrOrderNotFound
		}
		if err != nil {
			return err
		}
		if order.Status == StatusCanceled {
			result = &cancelRecord{Order: order, Replayed: true}
			return nil
		}
		if !canOrderTransition(order.Status, StatusCanceled) {
			return ErrOrderStateConflict
		}

		item, err := findOrderItem(db, order.ID)
		if err != nil {
			return err
		}
		at := canceledAt.UTC()
		updated := db.Model(&Order{}).
			Where("id = ? AND status = ?", order.ID, string(StatusPendingPayment)).
			Updates(map[string]any{
				"status": string(StatusCanceled), "canceled_at": at, "updated_at": at,
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrOrderStateConflict
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
			Reason: "ORDER_CANCELED", BizID: order.OrderNo,
		}
		if err := db.Create(&ledger).Error; err != nil {
			return translateError(err)
		}
		if err := restoreUserCoupon(db, order, at); err != nil {
			return err
		}
		order.Status = StatusCanceled
		order.CanceledAt = &at
		order.UpdatedAt = at
		result = &cancelRecord{Order: order}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *GormRepository) ExpireNext(ctx context.Context, now time.Time) (bool, error) {
	expired := false
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		var order Order
		err := db.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND expires_at <= ?", string(StatusPendingPayment), now.UTC()).
			Order("expires_at ASC, id ASC").Take(&order).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := expireLockedOrder(db, order, now.UTC()); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}

// expireLockedOrder 把已锁定的待支付订单关单：状态条件更新 + 库存回补 + 流水，
// 三者在调用方的事务里一起提交或一起回滚。
//
// 它同时被两条路径复用：
//   - 超时关单 Worker（批量兜底扫描）；
//   - 支付路径发现订单已过期时的"顺手关单"（lazy expiry）。
//
// 两条路径都以 `WHERE status = 'PENDING_PAYMENT'` 为条件更新依据，
// 因此谁先提交谁生效，另一方拿到 RowsAffected=0 并返回状态冲突，
// 库存回补永远不会发生两次。
func expireLockedOrder(db *gorm.DB, order Order, at time.Time) error {
	item, err := findOrderItem(db, order.ID)
	if err != nil {
		return err
	}
	updated := db.Model(&Order{}).
		Where("id = ? AND status = ?", order.ID, string(StatusPendingPayment)).
		Updates(map[string]any{"status": string(StatusExpired), "expired_at": at, "updated_at": at})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrOrderStateConflict
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
	ledger := inventoryLedger{SKUID: item.SKUID, OrderID: order.ID, Change: int64(item.Quantity), Reason: "ORDER_EXPIRED", BizID: order.OrderNo}
	if err := db.Create(&ledger).Error; err != nil {
		return translateError(err)
	}
	return restoreUserCoupon(db, order, at)
}

func findPaymentByIdentity(db *gorm.DB, paymentNo, providerTradeNo string) (*Payment, error) {
	var payments []Payment
	err := db.Where("payment_no = ? OR provider_trade_no = ?", paymentNo, providerTradeNo).
		Limit(2).Find(&payments).Error
	if err != nil {
		return nil, err
	}
	if len(payments) == 0 {
		// 未支付订单没有成功支付记录，是正常状态。
		return nil, nil //nolint:nilnil // 见上方注释：不存在不是错误
	}
	if len(payments) != 1 {
		return nil, ErrIdempotencyConflict
	}
	return &payments[0], nil
}

func sameSuccessfulPayment(payment Payment, orderID int64, cmd PayCommand) bool {
	return payment.OrderID == orderID && payment.PaymentNo == cmd.PaymentNo &&
		payment.ProviderTradeNo != nil && *payment.ProviderTradeNo == cmd.ProviderTradeNo &&
		payment.Amount == cmd.Amount && payment.Status == PaymentStatusSuccess && payment.PaidAt != nil
}

func translatePaymentWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "payments_payment_no_key" ||
			pgErr.ConstraintName == "idx_payments_provider_trade_no_unique") {
		return ErrIdempotencyConflict
	}
	return err
}

// applyUserCoupon 锁住持券行并校验它能不能用于这笔订单，返回优惠金额。
//
// 这里读的是 coupon 模块的表（order 自己的 read model + 直接 SQL），
// 但**折扣规则调用 coupon.DiscountFor**——规则只能有一份实现，
// 否则"下单算一次、视图显示一次、对账再算一次"必然漂移（DomTok 的教训）。
func applyUserCoupon(db *gorm.DB, userID, merchantID, userCouponID int64, orderAmount int64, now time.Time) (int64, error) {
	var row struct {
		UserCouponID int64     `gorm:"column:id"`
		CouponID     int64     `gorm:"column:coupon_id"`
		MerchantID   int64     `gorm:"column:merchant_id"`
		Status       string    `gorm:"column:status"`
		Title        string    `gorm:"column:title"`
		Threshold    int64     `gorm:"column:threshold_amount"`
		Discount     int64     `gorm:"column:discount_amount"`
		ValidFrom    time.Time `gorm:"column:valid_from"`
		ValidTo      time.Time `gorm:"column:valid_to"`
	}
	err := db.Raw(`
		SELECT uc.id, uc.coupon_id, uc.status, c.merchant_id, c.title, c.threshold_amount,
		       c.discount_amount, c.valid_from, c.valid_to
		FROM user_coupons uc
		JOIN coupons c ON c.id = uc.coupon_id
		WHERE uc.id = ? AND uc.user_id = ?
		FOR UPDATE OF uc`, userCouponID, userID).Scan(&row).Error
	if err != nil {
		return 0, err
	}
	if row.UserCouponID == 0 {
		// 券不存在与券不属于当前用户返回同一错误，不泄露存在性。
		return 0, ErrCouponNotFound
	}
	if row.MerchantID != merchantID {
		return 0, ErrCouponNotUsable
	}
	discount, err := coupon.DiscountFor(
		coupon.UserCoupon{ID: row.UserCouponID, CouponID: row.CouponID, UserID: userID, Status: coupon.UserCouponStatus(row.Status)},
		coupon.Coupon{ID: row.CouponID, Title: row.Title, ThresholdAmount: row.Threshold, DiscountAmount: row.Discount, ValidFrom: row.ValidFrom, ValidTo: row.ValidTo},
		orderAmount, now.UTC(),
	)
	if err != nil {
		return 0, translateCouponError(err)
	}
	return discount, nil
}

// translateCouponError 把券域的哨兵翻译成订单域的错误，避免订单模块
// 把 coupon 包的内部类型泄露给 handler。
func translateCouponError(err error) error {
	switch {
	case errors.Is(err, coupon.ErrUserCouponUsed):
		return ErrCouponConflict
	case errors.Is(err, coupon.ErrCouponExpired):
		return ErrCouponNotUsable
	case errors.Is(err, coupon.ErrThresholdNotMet):
		return ErrCouponThresholdNotMet
	default:
		return err
	}
}

type userCouponRow struct {
	ID int64 `gorm:"column:id;primaryKey"`
}

func (userCouponRow) TableName() string { return "user_coupons" }

func findExistingOrder(db *gorm.DB, userID int64, key string) (*Order, error) {
	var existing Order
	err := db.Where("user_id = ? AND idempotency_key = ?", userID, key).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 幂等键没有对应订单是正常状态（首次下单），不是错误。
		return nil, nil //nolint:nilnil // 见上方注释：不存在不是错误
	}
	if err != nil {
		return nil, err
	}
	return &existing, nil
}

func findOrderItem(db *gorm.DB, orderID int64) (*OrderItem, error) {
	var item OrderItem
	if err := db.Where("order_id = ?", orderID).Take(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("order invariant violated: existing order has no item")
		}
		return nil, err
	}
	return &item, nil
}

func findSellableSKU(db *gorm.DB, skuID int64) (*SellableSKU, error) {
	var row SellableSKU
	err := db.Raw(`
		SELECT s.id AS sku_id, s.product_id, p.merchant_id,
		       p.title AS product_title, s.spec AS sku_spec, s.price AS unit_price
		FROM skus s
		JOIN products p ON p.id = s.product_id
		WHERE s.id = ? AND s.status = 'ACTIVE' AND p.status = 'ON_SALE'
		FOR SHARE OF s, p`, skuID).Scan(&row).Error
	if err != nil {
		return nil, translateError(err)
	}
	if row.SKUID == 0 {
		return nil, ErrNotFound
	}
	return &row, nil
}

func requireActiveSource(db *gorm.DB, source Source) error {
	var row struct {
		ID int64 `gorm:"column:id"`
	}
	err := db.Raw(`
		SELECT id FROM video_products
		WHERE video_id = ? AND author_id = ? AND product_id = ? AND status = 'ACTIVE'
		FOR SHARE`, source.VideoID, source.AuthorID, source.ProductID).Scan(&row).Error
	if err != nil {
		return err
	}
	if row.ID == 0 {
		return ErrNotFound
	}
	return nil
}

func multiplyAmount(price Amount, quantity int32) (Amount, bool) {
	if price <= 0 || quantity <= 0 || int64(price) > math.MaxInt64/int64(quantity) {
		return 0, false
	}
	return price * Amount(quantity), true
}

func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

type inventoryRow struct {
	SKUID int64 `gorm:"column:sku_id;primaryKey"`
}

func (inventoryRow) TableName() string { return "inventory" }

type inventoryLedger struct {
	ID      int64  `gorm:"column:id;primaryKey"`
	SKUID   int64  `gorm:"column:sku_id"`
	OrderID int64  `gorm:"column:order_id"`
	Change  int64  `gorm:"column:change"`
	Reason  string `gorm:"column:reason"`
	BizID   string `gorm:"column:biz_id"`
}

func (inventoryLedger) TableName() string { return "inventory_transactions" }

func int64ptr(v int64) *int64 { return &v }
