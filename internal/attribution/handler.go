package attribution

import (
	"context"
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
	g.GET("/authors/:id/sales", h.sales)
}

func (h *Handler) sales(ctx context.Context, c *app.RequestContext) {
	authorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || authorID <= 0 {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	// There is no public revenue policy in v1. Do not reveal another author's
	// order facts, including whether they have sales.
	if authorID != c.GetInt64(middleware.CtxUserIDKey) {
		response.Error(c, response.ErrUserNotFound)
		return
	}
	sales, err := h.service.GetSales(ctx, authorID)
	if err != nil {
		slog.Error("get author sales failed", "author_id", authorID, "err", err)
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, sales)
}
