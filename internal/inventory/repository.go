package inventory

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

var (
	ErrNotFound          = errors.New("inventory record not found")
	ErrInsufficientStock = errors.New("insufficient stock")
	ErrDuplicateBiz      = errors.New("duplicate inventory transaction")
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

// Adjust 条件更新 + 流水写入在同一事务：
//   - 条件 quantity + change >= 0 防止负库存（超卖的镜像问题），未命中即库存不足；
//   - 流水 biz_id 唯一冲突翻译为 ErrDuplicateBiz，由 service 决定幂等语义；
//   - 任意一步失败整体回滚，保证"数字变了必有流水"。
//
// 已知边界：inventory 行按需惰性创建（首次正向调整时补建），并发首调存在极小概率
// 主键冲突报 500，商家人工操作场景可忽略；第三阶段接订单时改为随 SKU 创建同步建行。
func (r *GormRepository) Adjust(ctx context.Context, tx *InventoryTransaction) error {
	return r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
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
			// 区分两种未命中：行不存在（惰性补建）vs 行存在但库存不足
			var count int64
			if err := db.Model(&Inventory{}).Where("sku_id = ?", tx.SkuID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				if tx.Change < 0 {
					return ErrInsufficientStock // 库存为 0 还要扣
				}
				if err := db.Create(&Inventory{
					SkuID:    tx.SkuID,
					Quantity: tx.Change,
				}).Error; err != nil {
					return err
				}
			} else {
				return ErrInsufficientStock
			}
		}
		if err := db.Create(tx).Error; err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrDuplicateBiz
			}
			return err
		}
		return nil
	})
}
