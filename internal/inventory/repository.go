package inventory

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNotFound          = errors.New("inventory record not found")
	ErrInsufficientStock = errors.New("insufficient stock")
)

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

func (r *GormRepository) GetBySkuID(ctx context.Context, skuID int64) (*Inventory, error) {
	var inv Inventory
	if err := r.db.WithContext(ctx).First(&inv, skuID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &inv, nil
}

// SkuOwner 归属校验用的只读投影：join 共享表拿 SKU 所属商品与商家。
// 直接 SQL 而非 import product 模块——模块间不共享类型，只读共享表允许直接查询，
// 避免 product <-> inventory 循环依赖。
type SkuOwner struct {
	ProductID  int64
	MerchantID int64
}

func (r *GormRepository) FindSkuOwner(ctx context.Context, skuID int64) (*SkuOwner, error) {
	var row struct {
		ProductID  int64 `gorm:"column:product_id"`
		MerchantID int64 `gorm:"column:merchant_id"`
	}
	err := r.db.WithContext(ctx).
		Table("skus s").
		Select("s.product_id AS product_id", "p.merchant_id AS merchant_id").
		Joins("JOIN products p ON p.id = s.product_id").
		Where("s.id = ?", skuID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &SkuOwner{ProductID: row.ProductID, MerchantID: row.MerchantID}, nil
}

// AdjustmentResult 描述库存流水的幂等仲裁结果。
// Existing 非 nil 时表示调用方提交的幂等范围已经由另一事务占用；service
// 必须比较业务载荷，只有完全相同的重试才可以当作成功。
type AdjustmentResult struct {
	Inserted bool
	Existing *InventoryTransaction
}

// Adjust 以一个短数据库事务完成幂等 claim、库存条件更新和流水写入。
//
// 先用 (sku_id, reason, biz_id) 的唯一约束抢占流水幂等 claim，再做库存
// 条件更新。这样并发重放在发现重复 key 时不会再次改库存；库存不足时
// 整个事务回滚，claim 也不会残留，客户端可以修正数量后重试新的 key。
//
// inventory 行由 product.CreateSKU 在 SKU 创建事务中同步初始化，因此这里
// 不再惰性补建。行不存在代表数据库不变量被破坏，返回 ErrNotFound，避免
// 把“库存表缺行”静默伪装成一次成功的入库。
func (r *GormRepository) Adjust(ctx context.Context, tx *InventoryTransaction) (*AdjustmentResult, error) {
	var result AdjustmentResult
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		claim := db.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "sku_id"},
				{Name: "reason"},
				{Name: "biz_id"},
			},
			DoNothing: true,
		}).Create(tx)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			var existing InventoryTransaction
			if err := db.Where(
				"sku_id = ? AND reason = ? AND biz_id = ?",
				tx.SkuID, string(tx.Reason), tx.BizID,
			).Take(&existing).Error; err != nil {
				return translateInventoryError(err)
			}
			result.Existing = &existing
			return nil
		}

		res := db.Model(&Inventory{}).
			Where("sku_id = ? AND quantity + ? >= 0", tx.SkuID, tx.Change).
			Updates(map[string]any{
				"quantity":   gorm.Expr("quantity + ?", tx.Change),
				"version":    gorm.Expr("version + 1"),
				"updated_at": time.Now(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 行存在但条件 quantity + change >= 0 未满足 = 库存不足；
			// 行不存在则是不变量破坏，不应通过创建行掩盖问题。
			var count int64
			if err := db.Model(&Inventory{}).Where("sku_id = ?", tx.SkuID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return ErrNotFound
			}
			return ErrInsufficientStock
		}
		result.Inserted = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func translateInventoryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
