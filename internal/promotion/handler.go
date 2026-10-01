package promotion

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

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) RegisterProtectedRoutes(g *route.RouterGroup) {
	g.POST("/videos/:id/products", h.link)
	g.PATCH("/videos/:id/products/:product_id", h.updateSort)
	g.DELETE("/videos/:id/products/:product_id", h.unlink)
}

func (h *Handler) RegisterOptionalRoutes(g *route.RouterGroup) {
	g.GET("/videos/:id/products", h.list)
}

type linkRequest struct {
	ProductID int64 `json:"product_id" binding:"required"`
	SortOrder int32 `json:"sort_order"                    vd:"$>=0"`
}
type sortRequest struct {
	SortOrder *int32 `json:"sort_order"`
}

func (h *Handler) link(ctx context.Context, c *app.RequestContext) {
	videoID, ok := parsePositive(c.Param("id"))
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req linkRequest
	if err := c.BindAndValidate(&req); err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Link(ctx, LinkParams{AuthorID: c.GetInt64(middleware.CtxUserIDKey), VideoID: videoID, ProductID: req.ProductID, SortOrder: req.SortOrder})
	h.respond(c, result, err)
}

func (h *Handler) updateSort(ctx context.Context, c *app.RequestContext) {
	videoID, vok := parsePositive(c.Param("id"))
	productID, pok := parsePositive(c.Param("product_id"))
	if !vok || !pok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req sortRequest
	if err := c.BindAndValidate(&req); err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	if req.SortOrder == nil || *req.SortOrder < 0 {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.UpdateSort(ctx, LinkParams{AuthorID: c.GetInt64(middleware.CtxUserIDKey), VideoID: videoID, ProductID: productID, SortOrder: *req.SortOrder})
	h.respond(c, result, err)
}

func (h *Handler) unlink(ctx context.Context, c *app.RequestContext) {
	videoID, vok := parsePositive(c.Param("id"))
	productID, pok := parsePositive(c.Param("product_id"))
	if !vok || !pok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Unlink(ctx, c.GetInt64(middleware.CtxUserIDKey), videoID, productID)
	h.respond(c, result, err)
}

func (h *Handler) list(ctx context.Context, c *app.RequestContext) {
	videoID, ok := parsePositive(c.Param("id"))
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	cards, err := h.service.ListCards(ctx, videoID)
	if err != nil {
		slog.Error("list video product cards", "video_id", videoID, "err", err)
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, map[string]any{"items": cards})
}

func (h *Handler) respond(c *app.RequestContext, result *LinkResult, err error) {
	switch {
	case err == nil:
		response.OK(c, result)
	case errors.Is(err, ErrInvalidParam):
		response.Error(c, response.ErrInvalidArgument)
	case errors.Is(err, ErrNotFound):
		response.Error(c, response.ErrVideoProductNotFound)
	case errors.Is(err, ErrActiveLimit):
		response.Error(c, response.ErrVideoProductLimit)
	default:
		slog.Error("video product command failed", "err", err)
		response.Error(c, response.ErrInternal)
	}
}

func parsePositive(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}
