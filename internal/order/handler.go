package order

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/lihualin/tokmall/internal/response"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) RegisterProtectedRoutes(g *route.RouterGroup) {
	g.POST("/orders", h.create)
}

// CreateOrderRequest 是 HTTP 边界 DTO。user_id 刻意不在请求体中，防止调用者
// 伪造身份；Handler 只从认证中间件写入的上下文读取当前用户。
type CreateOrderRequest struct {
	SKUID          int64              `json:"sku_id" binding:"required"`
	Quantity       int32              `json:"quantity" binding:"required"`
	IdempotencyKey string             `json:"idempotency_key" binding:"required"`
	Source         *CreateOrderSource `json:"source"`
}

type CreateOrderSource struct {
	VideoID   int64 `json:"video_id" binding:"required"`
	AuthorID  int64 `json:"author_id" binding:"required"`
	ProductID int64 `json:"product_id" binding:"required"`
}

type CreateOrderResponse struct {
	OrderID     int64             `json:"order_id"`
	OrderNo     string            `json:"order_no"`
	Status      Status            `json:"status"`
	TotalAmount Amount            `json:"total_amount"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Item        OrderItemResponse `json:"item"`
	Replayed    bool              `json:"replayed"`
}

type OrderItemResponse struct {
	ProductID    int64  `json:"product_id"`
	SKUID        int64  `json:"sku_id"`
	MerchantID   int64  `json:"merchant_id"`
	ProductTitle string `json:"product_title"`
	SKUSpec      string `json:"sku_spec"`
	UnitPrice    Amount `json:"unit_price"`
	Quantity     int32  `json:"quantity"`
	LineAmount   Amount `json:"line_amount"`
}

func (h *Handler) create(ctx context.Context, c *app.RequestContext) {
	var req CreateOrderRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind order request failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	params := CreateParams{
		UserID: c.GetInt64(middleware.CtxUserIDKey), SKUID: req.SKUID,
		Quantity: req.Quantity, IdempotencyKey: req.IdempotencyKey,
	}
	if req.Source != nil {
		params.Source = &Source{
			VideoID: req.Source.VideoID, AuthorID: req.Source.AuthorID,
			ProductID: req.Source.ProductID,
		}
	}
	result, err := h.service.Create(ctx, params)
	if err != nil {
		h.mapError(c, err)
		return
	}
	// 首次创建与幂等重放使用同一状态码和响应结构，客户端重试不需要分支处理。
	response.Created(c, mapCreateResponse(result))
}

func (h *Handler) mapError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, ErrInvalidParam):
		response.Error(c, response.ErrInvalidArgument)
	case errors.Is(err, ErrNotFound):
		// 不区分 SKU 不可售与来源关系无效，避免泄露不可见业务对象。
		response.Error(c, response.ErrProductNotFound)
	case errors.Is(err, ErrInsufficientStock):
		response.Error(c, response.ErrInsufficientStock)
	case errors.Is(err, ErrIdempotencyConflict):
		response.Error(c, response.ErrIdempotencyConflict)
	default:
		slog.Error("create order internal error", "err", err)
		response.Error(c, response.ErrInternal)
	}
}

func mapCreateResponse(result *CreateResult) CreateOrderResponse {
	return CreateOrderResponse{
		OrderID: result.OrderID, OrderNo: result.OrderNo, Status: result.Status,
		TotalAmount: result.TotalAmount, ExpiresAt: result.ExpiresAt, Replayed: result.Replayed,
		Item: OrderItemResponse{
			ProductID: result.Item.ProductID, SKUID: result.Item.SKUID,
			MerchantID: result.Item.MerchantID, ProductTitle: result.Item.ProductTitle,
			SKUSpec: result.Item.SKUSpec, UnitPrice: result.Item.UnitPrice,
			Quantity: result.Item.Quantity, LineAmount: result.Item.LineAmount,
		},
	}
}
