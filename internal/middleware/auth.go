package middleware

import (
	"context"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/lihualin/tokmall/internal/response"
	"github.com/lihualin/tokmall/internal/token"
)

const (
	CtxUserIDKey = "user_id"
	CtxRoleKey   = "role"
)

func AuthMiddleware(manager *token.Manager) app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		authHeader := string(ctx.GetHeader("Authorization"))
		if authHeader == "" {
			response.Abort(ctx, response.ErrMissingAuthHeader)
			return
		}
		authHeaderParts := strings.Split(authHeader, " ")
		if len(authHeaderParts) < 2 {
			response.Abort(ctx, response.ErrInvalidAuthType)
			return
		}
		if authHeaderParts[0] != "Bearer" {
			response.Abort(ctx, response.ErrInvalidAuthType)
			return
		}
		claims, err := manager.Verify(authHeaderParts[1])
		if err != nil {
			response.Abort(ctx, response.ErrInvalidToken)
			return
		}
		if !claims.IsAccess() {
			response.Abort(ctx, response.ErrInvalidToken)
			return
		}
		userID, err := strconv.ParseInt(claims.Subject, 10, 64)
		if err != nil {
			response.Abort(ctx, response.ErrInvalidToken)
			return
		}
		ctx.Set(CtxUserIDKey, userID)
		ctx.Set(CtxRoleKey, claims.Role)
		ctx.Next(c)
	}
}
