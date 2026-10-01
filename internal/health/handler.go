package health

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"github.com/lihualin/tokmall/internal/response"
)

type Handler struct {
	db *gorm.DB
}

func NewHandler(db *gorm.DB) *Handler {
	return &Handler{
		db: db,
	}
}

// Register 将 health 相关路由挂载到 Hertz 引擎上。
func (hd *Handler) Register(h *server.Hertz) {
	healthGroup := h.Group("/health")
	healthGroup.GET("/live", hd.live)
	healthGroup.GET("/ready", hd.ready)
}

func (hd *Handler) live(ctx context.Context, c *app.RequestContext) {
	response.OK(c, healthStatus{Status: "ok"})
}

func (hd *Handler) ready(ctx context.Context, c *app.RequestContext) {
	sqlDB, err := hd.db.DB()
	if err != nil {
		response.Error(c, response.ErrDependencyUnavailable)
		return
	}
	err = sqlDB.Ping()
	if err != nil {
		response.Error(c, response.ErrDependencyUnavailable)
		return
	}
	response.OK(c, healthStatus{Status: "ok"})
}

type healthStatus struct {
	Status string `json:"status"`
}
