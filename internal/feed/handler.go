package feed

import (
	"context"
	"errors"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/lihualin/tokmall/internal/response"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes 挂公开路由：Feed 匿名可看，不要求登录。
func (h *Handler) RegisterRoutes(g *route.RouterGroup) {
	g.GET("/feed", h.list)
}

func (h *Handler) list(ctx context.Context, c *app.RequestContext) {
	limitRaw := c.Query("limit")
	var limit int
	if limitRaw != "" {
		n, err := strconv.Atoi(limitRaw)
		if err != nil {
			response.Error(c, response.ErrInvalidArgument)
			return
		}
		limit = n
	}
	result, err := h.service.List(ctx, limit, c.Query("cursor"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidLimit):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrInvalidCursor):
			response.Error(c, response.ErrInvalidCursor)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
}
