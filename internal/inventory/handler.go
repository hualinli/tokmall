package inventory

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/lihualin/tokmall/internal/response"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterProtectedRoutes 商家侧库存端点，挂在 /products/:id/skus/:sku_id/stock 下，
// 与 product 模块的 SKU 端点同前缀但路径不同，路由树互不冲突。
func (h *Handler) RegisterProtectedRoutes(g *route.RouterGroup) {
	g.GET("/products/:id/skus/:sku_id/stock", h.getStock)
	g.POST("/products/:id/skus/:sku_id/stock", h.adjustStock)
}

type AdjustStockRequest struct {
	Change         int64  `json:"change" vd:"$!=0"` // 正数入库，负数扣减
	IdempotencyKey string `json:"idempotency_key" vd:"len($)>0"`
}

func (h *Handler) getStock(ctx context.Context, c *app.RequestContext) {
	merchantID, productID, skuID, ok := parsePathParams(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	view, err := h.service.GetStock(ctx, merchantID, productID, skuID)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, view)
}

func (h *Handler) adjustStock(ctx context.Context, c *app.RequestContext) {
	merchantID, productID, skuID, ok := parsePathParams(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req AdjustStockRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	view, err := h.service.AdjustStock(ctx, merchantID, productID, skuID, req.Change, req.IdempotencyKey)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, view)
}

func (h *Handler) mapError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, ErrInvalidParam):
		response.Error(c, response.ErrInvalidArgument)
	case errors.Is(err, ErrNotFound):
		// SKU 不存在 / 不属于该商品 / 不属于调用商家，统一 404
		response.Error(c, response.ErrProductNotFound)
	case errors.Is(err, ErrInsufficientStock):
		response.Error(c, response.ErrInsufficientStock)
	default:
		slog.Error("inventory internal error", "err", err)
		response.Error(c, response.ErrInternal)
	}
}

func parsePathParams(c *app.RequestContext) (merchantID, productID, skuID int64, ok bool) {
	merchantID = c.GetInt64(middleware.CtxUserIDKey)
	productID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	skuID, err = strconv.ParseInt(c.Param("sku_id"), 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	return merchantID, productID, skuID, true
}
