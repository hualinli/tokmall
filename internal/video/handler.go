package video

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/hertz-contrib/requestid"
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

// PlayRequest 播放事件上报。EventID 客户端生成标准 UUID（v4/v7 均接受，幂等去重）；
// PlayedAt 是客户端事件时间（RFC3339），缺省服务端 now()。
type PlayRequest struct {
	EventID   string     `json:"event_id" binding:"required"`
	SessionID *string    `json:"session_id"`
	PlayedAt  *time.Time `json:"played_at"`
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
	videos.GET("/me", h.listMine)
	videos.POST("/:id/publish", h.publish)
	videos.POST("/:id/play", h.play)
	videos.DELETE("/:id", h.delete)
}

// RegisterOptionalRoutes 视频详情公开可看（匿名 is_liked=false）：
// 可见性策略由 service 判定（未发布仅作者，匿名一律 404）。
func (h *Handler) RegisterOptionalRoutes(g *route.RouterGroup) {
	g.GET("/videos/:id", h.get)
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
		case errors.Is(err, ErrStorageUnavailable):
			// 熔断打开：依赖暂时不可用，返回 503 让客户端退避重试，
			// 而不是 500（那会被当成服务端 bug 计入错误率）。
			response.Error(c, response.ErrDependencyUnavailable)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.Created(c, result)
}

func (h *Handler) listMine(ctx context.Context, c *app.RequestContext) {
	limit := 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			response.Error(c, response.ErrInvalidArgument)
			return
		}
		limit = n
	}
	result, err := h.service.ListMine(ctx, h.userID(c), limit, c.Query("cursor"))
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
		case errors.Is(err, ErrNotFound):
			response.Error(c, response.ErrVideoNotFound)
		case errors.Is(err, ErrStatusConflict):
			response.Error(c, response.ErrDuplicateAction)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.OK(c, result)
}

// play 上报播放事件（受保护）：事件异步语义的同步落库版（M5 前）。
// 成功返回 204；幂等重放（同一 event_id）同样 204，不重复计数。
func (h *Handler) play(ctx context.Context, c *app.RequestContext) {
	videoID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req PlayRequest
	if err := c.BindAndValidate(&req); err != nil {
		slog.Debug("bind failed", "err", err)
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var sessionID, traceID *string
	if req.SessionID != nil && *req.SessionID != "" {
		sessionID = req.SessionID
	}
	if rid := requestid.Get(c); rid != "" {
		traceID = &rid
	}
	if err := h.service.Play(ctx, h.userID(c), videoID, req.EventID, sessionID, req.PlayedAt, traceID); err != nil {
		switch {
		case errors.Is(err, ErrInvalidParam):
			response.Error(c, response.ErrInvalidArgument)
		case errors.Is(err, ErrNotFound):
			response.Error(c, response.ErrVideoNotFound)
		case errors.Is(err, ErrIdempotencyConflict):
			response.Error(c, response.ErrIdempotencyConflict)
		default:
			response.Error(c, response.ErrInternal)
		}
		return
	}
	response.NoContent(c)
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
