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

// RegisterOptionalRoutes 关注/粉丝列表：公开读 + 可选认证（is_following 个性化）。
func (h *Handler) RegisterOptionalRoutes(g *route.RouterGroup) {
	users := g.Group("/users")
	users.GET("/:id/following", h.followingList)
	users.GET("/:id/followers", h.followersList)
}

func (h *Handler) followingList(ctx context.Context, c *app.RequestContext) {
	h.followList(ctx, c, h.service.ListFollowing)
}

func (h *Handler) followersList(ctx context.Context, c *app.RequestContext) {
	h.followList(ctx, c, h.service.ListFollowers)
}

func (h *Handler) followList(ctx context.Context, c *app.RequestContext, fn func(context.Context, int64, int64, int, string) (*ProfileListResult, error)) {
	userID, ok := pathID(c)
	if !ok {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			response.Error(c, response.ErrInvalidArgument)
			return
		}
		limit = n
	}
	result, err := fn(ctx, h.userID(c), userID, limit, c.Query("cursor"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam), errors.Is(err, ErrInvalidCursor):
			response.Error(c, response.ErrInvalidArgument)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
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
