package interaction

import (
	"context"
	"errors"
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
	videos := g.Group("/videos")
	videos.POST("/:id/like", h.like)
	videos.DELETE("/:id/like", h.unlike)
	users := g.Group("/users")
	users.POST("/:id/follow", h.follow)
	users.DELETE("/:id/follow", h.unfollow)
}

func (h *Handler) userID(c *app.RequestContext) int64 {
	return c.GetInt64(middleware.CtxUserIDKey)
}

func pathID(c *app.RequestContext) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func (h *Handler) like(ctx context.Context, c *app.RequestContext) {
	videoID, ok := pathID(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Like(ctx, h.userID(c), videoID)
	if err != nil {
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, result)
}

func (h *Handler) unlike(ctx context.Context, c *app.RequestContext) {
	videoID, ok := pathID(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Unlike(ctx, h.userID(c), videoID)
	if err != nil {
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, result)
}

func (h *Handler) follow(ctx context.Context, c *app.RequestContext) {
	followeeID, ok := pathID(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Follow(ctx, h.userID(c), followeeID)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrUserNotFound):
			response.Error(c, response.ErrUserNotFound)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
}

func (h *Handler) unfollow(ctx context.Context, c *app.RequestContext) {
	followeeID, ok := pathID(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Unfollow(ctx, h.userID(c), followeeID)
	if err != nil {
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, result)
}
