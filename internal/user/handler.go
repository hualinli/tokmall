package user

import (
	"context"
	"errors"
	"log/slog"

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
