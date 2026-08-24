package product

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

func (h *Handler) RegisterProtectedRoutes(g *route.RouterGroup) {
	products := g.Group("/products")
	products.POST("", h.create)
	products.POST("/:id/status", h.changeStatus)


	products.GET("", h.list)
}

func (h *Handler) RegisterOptionalRoutes(g *route.RouterGroup) {
	products := g.Group("/products")
	products.GET("/:id", h.get)
}

type CreateProductRequest struct {
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
	CoverKey    string `json:"cover_key"`
}

func (h *Handler) create(ctx context.Context, c *app.RequestContext) {
	var req CreateProductRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	p := CreateProductParams{
		MerchantID:  c.GetInt64(middleware.CtxUserIDKey),
		Title:       req.Title,
		Description: req.Description,
		CoverKey:    req.CoverKey,
	}
	result, err := h.service.CreateProduct(ctx, p)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
}

func (h *Handler) get(ctx context.Context, c *app.RequestContext) {
	productID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	detail, err := h.service.GetProduct(ctx, productID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			response.Error(c, response.ErrProductNotFound)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, detail)
}

type ChangeStatusRequest struct {
	Status string `json:"status" vd:"len($)>0 && in($,'ON_SALE','OFF_SALE','DELETED')"`
}

func (h *Handler) changeStatus(ctx context.Context, c *app.RequestContext) {
	productID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req ChangeStatusRequest
	if err := c.BindAndValidate(&req); err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.ChangeProductStatus(ctx, c.GetInt64(middleware.CtxUserIDKey), productID, ProductStatus(req.Status))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrNotFound):
			// 不存在与不属于调用者统一为 404，不泄露商品存在性
			response.Error(c, response.ErrProductNotFound)
		case errors.Is(err, ErrInvalidStatusTransition):
			response.Error(c, response.ErrInvalidArgument)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
}

func (h *Handler) list(ctx context.Context, c *app.RequestContext) {
	response.Error(c, response.ErrForbidden)
}
