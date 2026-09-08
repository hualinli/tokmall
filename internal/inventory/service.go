package inventory

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

var ErrInvalidParam = errors.New("invalid param")

// Repository 由 service 定义，GormRepository 隐式满足。
type Repository interface {
	GetBySkuID(ctx context.Context, skuID int64) (*Inventory, error)
	FindSkuOwner(ctx context.Context, skuID int64) (*SkuOwner, error)
	Adjust(ctx context.Context, tx *InventoryTransaction) error
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
			// 行未建 = 从未入过库，按 0 展示（惰性初始化的读侧语义）
			return &StockView{SkuID: skuID, Quantity: 0}, nil
		}
		return nil, err
	}
	return &StockView{SkuID: skuID, Quantity: inv.Quantity, UpdatedAt: inv.UpdatedAt}, nil
}

// AdjustStock 商家调整库存（正数入库、负数扣减），以 idempotency_key 幂等：
// 同一 key 重复提交不重复变更，返回当前库存——重试安全的语义由这里收敛。
func (s *Service) AdjustStock(ctx context.Context, merchantID, productID, skuID int64, change int64, idempotencyKey string) (*StockView, error) {
	if merchantID <= 0 || productID <= 0 || skuID <= 0 || change == 0 || strings.TrimSpace(idempotencyKey) == "" {
		return nil, ErrInvalidParam
	}
	if err := s.checkOwnership(ctx, merchantID, productID, skuID); err != nil {
		return nil, err
	}
	tx := &InventoryTransaction{
		SkuID:  skuID,
		Change: change,
		Reason: ReasonAdjust,
		BizID:  "adjust:" + idempotencyKey,
	}
	err := s.repo.Adjust(ctx, tx)
	if err != nil {
		if errors.Is(err, ErrDuplicateBiz) {
			// 幂等重放：同一 key 已经生效过，直接返回当前库存
			slog.Info("duplicate stock adjust ignored", "sku_id", skuID, "biz_id", tx.BizID)
			return s.GetStock(ctx, merchantID, productID, skuID)
		}
		return nil, err
	}
	return s.GetStock(ctx, merchantID, productID, skuID)
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
