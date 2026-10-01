package coupon

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

// RegisterOptionalRoutes 挂公开可读端点（匿名也能看有哪些券可领）。
func (h *Handler) RegisterOptionalRoutes(g *route.RouterGroup) {
	g.GET("/coupons/available", h.listAvailable)
}

// RegisterProtectedRoutes 挂需要登录的端点。
func (h *Handler) RegisterProtectedRoutes(g *route.RouterGroup) {
	g.POST("/coupons", h.create)
	g.GET("/coupons/mine", h.listMerchantCoupons)
	g.POST("/coupons/:id/claim", h.claim)
	g.GET("/me/coupons", h.listMyCoupons)
	g.GET("/me/coupons/:id", h.getMyCoupon)
}

type CreateCouponRequest struct {
	Title           string `json:"title"            binding:"required"`
	ThresholdAmount int64  `json:"threshold_amount" binding:"required"`
	DiscountAmount  int64  `json:"discount_amount"  binding:"required"`
	// TotalQuantity 为 0 表示不限量。
	TotalQuantity int64 `json:"total_quantity"`
	PerUserLimit  int32 `json:"per_user_limit"`
	// 有效期用 RFC3339 字符串，避免各语言时间戳精度差异。
	ValidFrom string `json:"valid_from" binding:"required"`
	ValidTo   string `json:"valid_to"   binding:"required"`
}

func (h *Handler) create(ctx context.Context, c *app.RequestContext) {
	var req CreateCouponRequest
	if err := c.BindAndValidate(&req); err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	validFrom, err1 := time.Parse(time.RFC3339, req.ValidFrom)
	validTo, err2 := time.Parse(time.RFC3339, req.ValidTo)
	if err1 != nil || err2 != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	perUserLimit := req.PerUserLimit
	if perUserLimit == 0 {
		perUserLimit = 1
	}
	view, err := h.service.CreateCoupon(ctx, CreateCouponParams{
		MerchantID: c.GetInt64(middleware.CtxUserIDKey), Title: req.Title,
		ThresholdAmount: req.ThresholdAmount, DiscountAmount: req.DiscountAmount,
		TotalQuantity: req.TotalQuantity, PerUserLimit: perUserLimit,
		ValidFrom: validFrom, ValidTo: validTo,
	})
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.Created(c, view)
}

func (h *Handler) listMerchantCoupons(ctx context.Context, c *app.RequestContext) {
	limit, ok := parseLimit(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	views, err := h.service.ListMerchantCoupons(ctx, c.GetInt64(middleware.CtxUserIDKey), limit)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, map[string]any{"items": views})
}

func (h *Handler) listAvailable(ctx context.Context, c *app.RequestContext) {
	limit, ok := parseLimit(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	// 匿名用户也能看券列表（viewerID=0 表示"没有领过任何券"）。
	views, err := h.service.ListAvailable(ctx, max(c.GetInt64(middleware.CtxUserIDKey), 1), limit)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, map[string]any{"items": views})
}

func (h *Handler) claim(ctx context.Context, c *app.RequestContext) {
	couponID, err := parseID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	view, err := h.service.Claim(ctx, c.GetInt64(middleware.CtxUserIDKey), couponID)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.Created(c, view)
}

func (h *Handler) listMyCoupons(ctx context.Context, c *app.RequestContext) {
	limit, ok := parseLimit(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	views, err := h.service.ListMyCoupons(ctx, c.GetInt64(middleware.CtxUserIDKey), c.Query("status"), limit)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, map[string]any{"items": views})
}

func (h *Handler) getMyCoupon(ctx context.Context, c *app.RequestContext) {
	id, err := parseID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	view, err := h.service.GetUserCoupon(ctx, c.GetInt64(middleware.CtxUserIDKey), id)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, view)
}

func parseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidParam
	}
	return id, nil
}

func parseLimit(c *app.RequestContext) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return 20, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 || limit > 100 {
		return 0, false
	}
	return limit, true
}

// mapError 把领域哨兵映射成统一错误码。未知错误一律 500 并记日志。
func (h *Handler) mapError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, ErrInvalidParam):
		response.Error(c, response.ErrInvalidArgument)
	case errors.Is(err, ErrCouponNotFound):
		response.Error(c, response.ErrCouponNotFound)
	case errors.Is(err, ErrUserCouponNotFound):
		response.Error(c, response.ErrUserCouponNotFound)
	case errors.Is(err, ErrSoldOut):
		response.Error(c, response.ErrCouponSoldOut)
	case errors.Is(err, ErrAlreadyClaimed):
		response.Error(c, response.ErrCouponAlreadyClaimed)
	case errors.Is(err, ErrCouponNotClaimable), errors.Is(err, ErrCouponExpired):
		response.Error(c, response.ErrCouponNotClaimable)
	case errors.Is(err, ErrUserCouponUsed):
		response.Error(c, response.ErrCouponAlreadyUsed)
	default:
		slog.Error("coupon command internal error", "err", err)
		response.Error(c, response.ErrInternal)
	}
}
