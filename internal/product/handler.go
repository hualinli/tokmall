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
	products.POST("/:id/skus", h.addSKU)
	products.POST("/:id/skus/:sku_id/status", h.changeSKUStatus)

	products.GET("", h.list)
}

func (h *Handler) RegisterOptionalRoutes(g *route.RouterGroup) {
	products := g.Group("/products")
	// 静态段 /search 优先于 :param，Hertz 路由树不会把 "search" 当成 id
	products.GET("/search", h.search)
	products.GET("/:id", h.get)
}

type CreateProductRequest struct {
	Title       string `json:"title"       binding:"required"`
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
		case errors.Is(err, ErrInvalidCoverKey):
			// 封面 key 伪造/过期/文件不存在（与 user 模块头像同一映射）
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
		case errors.Is(err, ErrStatusConflict):
			// 条件更新未命中 = 状态已被并发修改，409 而非 500
			response.Error(c, response.ErrDuplicateAction)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
}

type AddSKURequest struct {
	Spec  string `json:"spec"`
	Price int64  `json:"price" vd:"$>0"` // 单位：分，必须为正
}

func (h *Handler) addSKU(ctx context.Context, c *app.RequestContext) {
	productID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req AddSKURequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	sku, err := h.service.AddSKU(ctx, AddSKUParams{
		ProductID:  productID,
		MerchantID: c.GetInt64(middleware.CtxUserIDKey),
		Spec:       req.Spec,
		PriceCents: req.Price,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrNotFound):
			response.Error(c, response.ErrProductNotFound)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.Created(c, sku)
}

type ChangeSKUStatusRequest struct {
	Status string `json:"status" vd:"len($)>0 && in($,'ACTIVE','DISABLED')"`
}

func (h *Handler) changeSKUStatus(ctx context.Context, c *app.RequestContext) {
	productID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	skuID, err := strconv.ParseInt(c.Param("sku_id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req ChangeSKUStatusRequest
	if err := c.BindAndValidate(&req); err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	sku, err := h.service.ChangeSKUStatus(
		ctx,
		c.GetInt64(middleware.CtxUserIDKey),
		productID,
		skuID,
		SKUStatus(req.Status),
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrNotFound):
			// SKU 不存在 / 不属于该商品 / 不属于调用者，统一 404
			response.Error(c, response.ErrProductNotFound)
		case errors.Is(err, ErrInvalidStatusTransition):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrStatusConflict):
			response.Error(c, response.ErrDuplicateAction)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, sku)
}

func (h *Handler) list(ctx context.Context, c *app.RequestContext) {
	beforeID, limit, err := parseListParams(c)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.ListProducts(ctx, ProductListParams{
		MerchantID: c.GetInt64(middleware.CtxUserIDKey),
		Status:     ProductStatus(c.Query("status")),
		Keyword:    c.Query("keyword"),
		BeforeID:   beforeID,
		Limit:      limit,
	})
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

func (h *Handler) search(ctx context.Context, c *app.RequestContext) {
	beforeID, limit, err := parseListParams(c)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.SearchProducts(ctx, c.Query("keyword"), beforeID, limit)
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

// parseListParams 解析 before_id / limit；未提供时返回零值，由 service 归一化。
func parseListParams(c *app.RequestContext) (int64, int, error) {
	var beforeID int64
	if v := c.Query("before_id"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, errors.New("invalid before_id")
		}
		beforeID = parsed
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 {
			return 0, 0, errors.New("invalid limit")
		}
		limit = parsed
	}
	return beforeID, limit, nil
}
