package health

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/lihualin/tokmall/internal/response"
)

// Register 将 health 相关路由挂载到 Hertz 引擎上。
func Register(h *server.Hertz) {
	healthGroup := h.Group("/health")
	healthGroup.GET("/live", live)
}

func live(ctx context.Context, c *app.RequestContext) {
	response.NoContent(c)
}
