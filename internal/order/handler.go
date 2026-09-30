package order

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
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
	g.GET("/orders", h.listBuyer)
	g.GET("/orders/:id", h.getBuyer)
	g.POST("/orders/:id/pay", h.pay)
	g.POST("/orders/:id/cancel", h.cancel)
	g.POST("/orders/:id/complete", h.complete)
	g.POST("/orders/:id/refund", h.requestRefund)
	g.GET("/orders/:id/refund", h.getBuyerRefund)
	g.GET("/merchant/orders", h.listMerchant)
	g.GET("/merchant/orders/:id", h.getMerchant)
	g.POST("/merchant/orders/:id/fulfill", h.fulfill)
	g.GET("/merchant/orders/:id/refund", h.getMerchantRefund)
	g.POST("/merchant/orders/:id/refund/approve", h.approveRefund)
	g.POST("/merchant/orders/:id/refund/reject", h.rejectRefund)
}

func (h *Handler) getBuyer(ctx context.Context, c *app.RequestContext)    { h.get(ctx, c, false) }
func (h *Handler) getMerchant(ctx context.Context, c *app.RequestContext) { h.get(ctx, c, true) }

func (h *Handler) get(ctx context.Context, c *app.RequestContext, merchant bool) {
	id, err := parseOrderID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	view, err := h.service.Get(ctx, c.GetInt64(middleware.CtxUserIDKey), id, merchant)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, view)
}

func (h *Handler) listBuyer(ctx context.Context, c *app.RequestContext)    { h.list(ctx, c, false) }
func (h *Handler) listMerchant(ctx context.Context, c *app.RequestContext) { h.list(ctx, c, true) }

func (h *Handler) list(ctx context.Context, c *app.RequestContext, merchant bool) {
	limit := 20
	beforeID := int64(0)
	var err error
	if raw := c.Query("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
	}
	if err == nil {
		if raw := c.Query("before_id"); raw != "" {
			beforeID, err = parseOrderID(raw)
		}
	}
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	page, err := h.service.List(ctx, c.GetInt64(middleware.CtxUserIDKey), beforeID, limit, merchant)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, page)
}

func (h *Handler) fulfill(ctx context.Context, c *app.RequestContext) {
	h.transition(ctx, c, true)
}

func (h *Handler) complete(ctx context.Context, c *app.RequestContext) {
	h.transition(ctx, c, false)
}

func (h *Handler) transition(ctx context.Context, c *app.RequestContext, merchant bool) {
	id, err := parseOrderID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	actorID := c.GetInt64(middleware.CtxUserIDKey)
	var result *TransitionResult
	if merchant {
		result, err = h.service.Fulfill(ctx, actorID, id)
	} else {
		result, err = h.service.Complete(ctx, actorID, id)
	}
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, result)
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

// PayOrderRequest 是学习环境中的用户主动模拟支付请求，不是支付渠道回调。
// 真实渠道回调必须使用独立入口并在进入领域服务前完成签名、时间窗与防重放校验。
type PayOrderRequest struct {
	PaymentNo       string `json:"payment_no" binding:"required"`
	ProviderTradeNo string `json:"provider_trade_no" binding:"required"`
	Amount          Amount `json:"amount" binding:"required"`
}

type PayOrderResponse struct {
	OrderID         int64     `json:"order_id"`
	OrderNo         string    `json:"order_no"`
	Status          Status    `json:"status"`
	PaymentNo       string    `json:"payment_no"`
	ProviderTradeNo string    `json:"provider_trade_no"`
	Amount          Amount    `json:"amount"`
	PaidAt          time.Time `json:"paid_at"`
	Replayed        bool      `json:"replayed"`
}

type CancelOrderResponse struct {
	OrderID    int64     `json:"order_id"`
	OrderNo    string    `json:"order_no"`
	Status     Status    `json:"status"`
	CanceledAt time.Time `json:"canceled_at"`
	Replayed   bool      `json:"replayed"`
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

func (h *Handler) pay(ctx context.Context, c *app.RequestContext) {
	orderID, err := parseOrderID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req PayOrderRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind simulated payment request failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Pay(ctx, PayParams{
		UserID: c.GetInt64(middleware.CtxUserIDKey), OrderID: orderID,
		PaymentNo: req.PaymentNo, ProviderTradeNo: req.ProviderTradeNo, Amount: req.Amount,
	})
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, PayOrderResponse{
		OrderID: result.OrderID, OrderNo: result.OrderNo, Status: result.Status,
		PaymentNo: result.PaymentNo, ProviderTradeNo: result.ProviderTradeNo,
		Amount: result.Amount, PaidAt: result.PaidAt, Replayed: result.Replayed,
	})
}

func (h *Handler) cancel(ctx context.Context, c *app.RequestContext) {
	orderID, err := parseOrderID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Cancel(ctx, CancelParams{
		UserID: c.GetInt64(middleware.CtxUserIDKey), OrderID: orderID,
	})
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, CancelOrderResponse{
		OrderID: result.OrderID, OrderNo: result.OrderNo, Status: result.Status,
		CanceledAt: result.CanceledAt, Replayed: result.Replayed,
	})
}

func parseOrderID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidParam
	}
	return id, nil
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
	case errors.Is(err, ErrOrderNotFound):
		// 不区分订单不存在与不属于当前用户，防止枚举他人订单。
		response.Error(c, response.ErrOrderNotFound)
	case errors.Is(err, ErrOrderStateConflict):
		response.Error(c, response.ErrOrderStateConflict)
	case errors.Is(err, ErrOrderExpired):
		response.Error(c, response.ErrOrderExpired)
	case errors.Is(err, ErrRefundNotFound):
		// 与订单同一语义：没有退款、或订单不属于当前调用者，对外都是"找不到"。
		response.Error(c, response.ErrRefundNotFound)
	case errors.Is(err, ErrRefundNotAllowed):
		response.Error(c, response.ErrRefundNotAllowed)
	case errors.Is(err, ErrRefundAlreadyDecided):
		response.Error(c, response.ErrRefundAlreadyDecided)
	case errors.Is(err, ErrPaymentAmountMismatch):
		response.Error(c, response.ErrPaymentAmountMismatch)
	default:
		slog.Error("order command internal error", "err", err)
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
