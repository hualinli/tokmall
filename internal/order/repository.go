package order

import (
	"context"
	"errors"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type GormRepository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *GormRepository { return &GormRepository{db: db} }

func (r *GormRepository) Create(ctx context.Context, cmd CreateCommand) (*createRecord, error) {
	var result *createRecord
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		// 已完成订单的重放不应重新依赖商品当前状态：商品在首单后下架，
		// 客户端仍必须能用原 key 拿到原结果。并发首次请求都未读到时，
		// 后面的唯一约束 claim 仍会完成最终仲裁。
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

		order := Order{
			OrderNo: cmd.OrderNo, UserID: cmd.UserID, MerchantID: sku.MerchantID,
			Status: StatusPendingPayment, TotalAmount: lineAmount,
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
		result = &createRecord{Order: order, Item: item}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func findExistingOrder(db *gorm.DB, userID int64, key string) (*Order, error) {
	var existing Order
	err := db.Where("user_id = ? AND idempotency_key = ?", userID, key).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
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
