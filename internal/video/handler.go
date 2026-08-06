package video

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

type UploadURLRequest struct {
	Filename    string `json:"filename" binding:"required"`
	FileSize    int64  `json:"file_size" binding:"required"`
	ContentType string `json:"content_type" binding:"required"`
}

type CreateVideoRequest struct {
	Title           string  `json:"title" binding:"required"`
	ObjectKey       string  `json:"object_key" binding:"required"`
	CoverKey        *string `json:"cover_key"`
	DurationSeconds int     `json:"duration_seconds" binding:"required"`
}

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterProtectedRoutes(g *route.RouterGroup) {
	videos := g.Group("/videos")
	videos.POST("/upload-url", h.uploadURL)
	videos.POST("", h.create)
	videos.GET("/:id", h.get)
	videos.POST("/:id/publish", h.publish)
	videos.DELETE("/:id", h.delete)
}

func (h *Handler) userID(c *app.RequestContext) int64 {
	return c.GetInt64(middleware.CtxUserIDKey)
}

func (h *Handler) uploadURL(ctx context.Context, c *app.RequestContext) {
	var req UploadURLRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.GetUploadURL(ctx, h.userID(c), req.Filename, req.FileSize, req.ContentType)
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

func (h *Handler) create(ctx context.Context, c *app.RequestContext) {
	var req CreateVideoRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Create(ctx, h.userID(c), CreateParams{
		Title:           req.Title,
		ObjectKey:       req.ObjectKey,
		CoverKey:        req.CoverKey,
		DurationSeconds: req.DurationSeconds,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrUploadURLExpired):
			response.Error(c, response.ErrUploadURLExpired)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.Created(c, result)
}

func (h *Handler) get(ctx context.Context, c *app.RequestContext) {
	videoID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	detail, err := h.service.Get(ctx, h.userID(c), videoID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			response.Error(c, response.ErrVideoNotFound)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, detail)
}

func (h *Handler) publish(ctx context.Context, c *app.RequestContext) {
	videoID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	result, err := h.service.Publish(ctx, h.userID(c), videoID)
	if err != nil {
		switch {
		case errors.Is(err, ErrStatusConflict):
			response.Error(c, response.ErrDuplicateAction)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
}

func (h *Handler) delete(ctx context.Context, c *app.RequestContext) {
	videoID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	err = h.service.Delete(ctx, h.userID(c), videoID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			response.Error(c, response.ErrVideoNotFound)
		case errors.Is(err, ErrForbidden):
			response.Error(c, response.ErrForbidden)
		case errors.Is(err, ErrStatusConflict):
			response.Error(c, response.ErrDuplicateAction)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.NoContent(c)
}
