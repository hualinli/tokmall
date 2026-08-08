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
	g.GET("/users/:id/videos", h.listByAuthor)
}

func (h *Handler) list(ctx context.Context, c *app.RequestContext) {
	result, err := h.service.List(ctx, queryLimit(c), c.Query("cursor"))
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

// listByAuthor 公开作者主页：已发布作品，FeedItem 结构（前端组件直接复用）。
func (h *Handler) listByAuthor(ctx context.Context, c *app.RequestContext) {
	authorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.ListByAuthor(ctx, authorID, queryLimit(c), c.Query("cursor"))
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

// queryLimit 解析 limit 参数，缺省 0（service 落默认值）。
func queryLimit(c *app.RequestContext) int {
	limitRaw := c.Query("limit")
	if limitRaw == "" {
		return 0
	}
	n, err := strconv.Atoi(limitRaw)
	if err != nil {
		return -1 // 触发 ErrInvalidLimit
	}
	return n
}
