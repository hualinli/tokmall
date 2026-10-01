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
		if !parseBearer(c, ctx, manager) {
			return
		}
		ctx.Next(c)
	}
}

// AuthOptional 可选认证：无 Authorization header → 匿名继续；
// 有 header 但无效（格式/签名/过期）→ 401。绝不静默降级为匿名——
// 客户端拿着自认为有效的 token 请求，拿到匿名数据会造成困惑。
// 消费方通过 CtxUserIDKey 取 0 判断匿名（Set 前先清空，防 header 污染）。
func AuthOptional(manager *token.Manager) app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		if string(ctx.GetHeader("Authorization")) == "" {
			ctx.Set(CtxUserIDKey, int64(0))
			ctx.Next(c)
			return
		}
		if !parseBearer(c, ctx, manager) {
			return
		}
		ctx.Next(c)
	}
}

// parseBearer 解析 Bearer access token 并注入 user_id/role。
// 失败时已 Abort 并返回 false。
func parseBearer(_ context.Context, ctx *app.RequestContext, manager *token.Manager) bool {
	authHeader := string(ctx.GetHeader("Authorization"))
	if authHeader == "" {
		response.Abort(ctx, response.ErrMissingAuthHeader)
		return false
	}
	authHeaderParts := strings.Split(authHeader, " ")
	if len(authHeaderParts) < 2 {
		response.Abort(ctx, response.ErrInvalidAuthType)
		return false
	}
	if authHeaderParts[0] != "Bearer" {
		response.Abort(ctx, response.ErrInvalidAuthType)
		return false
	}
	claims, err := manager.Verify(authHeaderParts[1])
	if err != nil {
		response.Abort(ctx, response.ErrInvalidToken)
		return false
	}
	if !claims.IsAccess() {
		response.Abort(ctx, response.ErrInvalidToken)
		return false
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		response.Abort(ctx, response.ErrInvalidToken)
		return false
	}
	ctx.Set(CtxUserIDKey, userID)
	ctx.Set(CtxRoleKey, claims.Role)
	return true
}
