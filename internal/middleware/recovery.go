package middleware

import (
	"context"
	"log/slog"

	"github.com/cloudwego/hertz/pkg/app"
	hertzrecovery "github.com/cloudwego/hertz/pkg/app/middlewares/server/recovery"

	"github.com/lihualin/tokmall/internal/response"
)

func Recovery() app.HandlerFunc {
	return hertzrecovery.Recovery(hertzrecovery.WithRecoveryHandler(func(c context.Context, ctx *app.RequestContext, err interface{}, stack []byte) {
		slog.Error("panic recovered", "err", err, "stack", string(stack))
		response.Error(ctx, response.ErrInternal)
	}))
}
