package user

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

type RegisterRequest struct {
	Email    string `json:"email" binding:"required"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type LoginRequest struct {
	Identifier string `json:"identifier" binding:"required"`
	Password   string `json:"password" binding:"required"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type LogoutRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type UpdateMeRequest struct {
	Bio       *string `json:"bio"`
	AvatarKey *string `json:"avatar_key"`
}

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(g *route.RouterGroup) {
	authGroup := g.Group("/auth")
	authGroup.POST("/register", h.register)
	authGroup.POST("/login", h.login)
	authGroup.POST("/refresh", h.refresh)
	authGroup.POST("/logout", h.logout)
}

func (h *Handler) RegisterProtectedRoutes(g *route.RouterGroup) {
	g.GET("/me", h.me)
	g.PATCH("/me", h.updateMe)
}

// RegisterOptionalRoutes 公开但带个性化字段的端点：有 token 注入 viewer_id，
// 没有就当匿名（AuthOptional）。
func (h *Handler) RegisterOptionalRoutes(g *route.RouterGroup) {
	g.GET("/users/:id", h.profile)
}

func (h *Handler) me(ctx context.Context, c *app.RequestContext) {
	userID := c.GetInt64(middleware.CtxUserIDKey)
	profile, err := h.service.GetProfile(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			response.Error(c, response.ErrUserNotFound)
			return
		}
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, profile)
}

func (h *Handler) updateMe(ctx context.Context, c *app.RequestContext) {
	var req UpdateMeRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	profile, err := h.service.UpdateMe(ctx, h.userID(c), UpdateMeParams{
		Bio:       req.Bio,
		AvatarKey: req.AvatarKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidImageKey):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrNotFound):
			response.Error(c, response.ErrUserNotFound)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, profile)
}

// userID 从中间件上下文取当前用户。
func (h *Handler) userID(c *app.RequestContext) int64 {
	return c.GetInt64(middleware.CtxUserIDKey)
}

func (h *Handler) profile(ctx context.Context, c *app.RequestContext) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	viewerID := c.GetInt64(middleware.CtxUserIDKey)
	profile, err := h.service.GetPublicProfile(ctx, viewerID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			response.Error(c, response.ErrUserNotFound)
			return
		}
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, profile)
}

func (h *Handler) register(ctx context.Context, c *app.RequestContext) {
	var req RegisterRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	params := req.toParams()
	authResult, err := h.service.Register(ctx, params)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
			return
		case errors.Is(err, ErrEmailTaken), errors.Is(err, ErrUsernameTaken):
			response.Error(c, response.ErrDuplicateAction)
			return
		default:
			response.Error(c, response.ErrInternal)
			return
		}
	}
	response.Created(c, authResult)
}

func (h *Handler) login(ctx context.Context, c *app.RequestContext) {
	var req LoginRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	params := req.toParams()
	authResult, err := h.service.Login(ctx, params)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			response.Error(c, response.ErrUnauthenticated)
			return
		}
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, authResult)
}

func (h *Handler) refresh(ctx context.Context, c *app.RequestContext) {
	var req RefreshRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	authResult, err := h.service.Refresh(ctx, req.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			response.Error(c, response.ErrUnauthenticated)
			return
		}
		response.Error(c, response.ErrInternal)
		return
	}
	response.OK(c, authResult)
}

func (h *Handler) logout(ctx context.Context, c *app.RequestContext) {
	var req LogoutRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	if err := h.service.Logout(ctx, req.RefreshToken); err != nil {
		response.Error(c, response.ErrInternal)
		return
	}
	response.NoContent(c)
}

func (r *RegisterRequest) toParams() RegisterParams {
	return RegisterParams{
		Username: r.Username,
		Password: r.Password,
		Email:    r.Email,
	}
}

func (r *LoginRequest) toParams() LoginParams {
	return LoginParams{
		EmailOrUsername: r.Identifier,
		Password:        r.Password,
	}
}
