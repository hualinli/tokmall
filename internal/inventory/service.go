package inventory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

var (
	ErrInvalidParam = errors.New("invalid param")
	// ErrIdempotencyConflict 表示同一 SKU/操作/key 已经绑定了另一组
	// 业务载荷，不能把唯一键冲突误报为幂等成功。
	ErrIdempotencyConflict = errors.New("idempotency key reused with different payload")
)

const (
	maxIdempotencyKeyLength = 128
	// maxStockChange 是单次调整的业务上界。没有上界时 quantity + change
	// 会在 BIGINT 上溢出（PostgreSQL 22003），把"参数非法"暴露成 500。
	maxStockChange = 1_000_000_000
)

// Repository 由 service 定义，GormRepository 隐式满足。
type Repository interface {
	GetBySkuID(ctx context.Context, skuID int64) (*Inventory, error)
	FindSkuOwner(ctx context.Context, skuID int64) (*SkuOwner, error)
	Adjust(ctx context.Context, tx *InventoryTransaction) (*AdjustmentResult, error)
}

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// StockView 对外暴露的库存视图（model 永不出 service）。
type StockView struct {
	SkuID     int64     `json:"sku_id"`
	Quantity  int64     `json:"quantity"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GetStock 商家查询 SKU 库存。归属链断裂一律 404，不泄露存在性。
func (s *Service) GetStock(ctx context.Context, merchantID, productID, skuID int64) (*StockView, error) {
	if err := s.checkOwnership(ctx, merchantID, productID, skuID); err != nil {
		return nil, err
	}
	inv, err := s.repo.GetBySkuID(ctx, skuID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 000011 会回填历史数据，新 SKU 也在创建事务中同步初始化。
			// 归属链存在但库存行缺失代表数据库不变量已被破坏，不能伪装成 0 库存。
			slog.Error("inventory row missing for existing sku", "sku_id", skuID)
			return nil, fmt.Errorf("inventory invariant violated: sku %d has no inventory row", skuID)
		}
		return nil, err
	}
	return &StockView{SkuID: skuID, Quantity: inv.Quantity, UpdatedAt: inv.UpdatedAt}, nil
}

// AdjustStock 商家调整库存（正数入库、负数扣减），以
// (sku_id, reason=ADJUST, idempotency_key) 定义幂等范围：
// 同一范围且同一业务载荷重试成功；复用 key 但变更数量或目标不一致返回
// ErrIdempotencyConflict。SKU 所属商家由 checkOwnership 绑定，因此不把
// merchant_id 冗余写进库存流水也不会造成跨商家假成功。
func (s *Service) AdjustStock(ctx context.Context, merchantID, productID, skuID int64, change int64, idempotencyKey string) (*StockView, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if merchantID <= 0 || productID <= 0 || skuID <= 0 || change == 0 || idempotencyKey == "" ||
		len(idempotencyKey) > maxIdempotencyKeyLength || change > maxStockChange || change < -maxStockChange {
		return nil, ErrInvalidParam
	}
	if err := s.checkOwnership(ctx, merchantID, productID, skuID); err != nil {
		return nil, err
	}
	tx := &InventoryTransaction{
		SkuID:  skuID,
		Change: change,
		Reason: ReasonAdjust,
		BizID:  idempotencyKey,
	}
	result, err := s.repo.Adjust(ctx, tx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("inventory repository returned nil adjustment result")
	}
	if !result.Inserted {
		if result.Existing == nil {
			return nil, errors.New("inventory repository returned nil existing transaction")
		}
		if !sameTransactionBusinessFact(tx, result.Existing) {
			slog.Warn("inventory idempotency conflict", "sku_id", skuID, "biz_id", tx.BizID)
			return nil, ErrIdempotencyConflict
		}
		// 幂等重放：同一 key、同一载荷已经生效过，直接返回当前库存。
		slog.Info("duplicate stock adjust replayed", "sku_id", skuID, "biz_id", tx.BizID)
	}
	return s.GetStock(ctx, merchantID, productID, skuID)
}

func sameTransactionBusinessFact(want, got *InventoryTransaction) bool {
	if want == nil || got == nil {
		return false
	}
	return want.SkuID == got.SkuID &&
		want.Change == got.Change &&
		want.Reason == got.Reason &&
		want.BizID == got.BizID &&
		((want.OrderID == nil && got.OrderID == nil) ||
			(want.OrderID != nil && got.OrderID != nil && *want.OrderID == *got.OrderID))
}

// checkOwnership 归属链：SKU 存在 -> 挂在指定商品下 -> 商品属于调用商家。
// 任一环断裂统一 ErrNotFound，不泄露存在性。
func (s *Service) checkOwnership(ctx context.Context, merchantID, productID, skuID int64) error {
	if merchantID <= 0 || productID <= 0 || skuID <= 0 {
		return ErrInvalidParam
	}
	owner, err := s.repo.FindSkuOwner(ctx, skuID)
	if err != nil {
		return err
	}
	if owner.ProductID != productID || owner.MerchantID != merchantID {
		return ErrNotFound
	}
	return nil
}
