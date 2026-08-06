package middleware

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hzconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lihualin/tokmall/internal/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSecret = []byte("test-secret-not-for-production")

// newAuthEngine 构造挂了 AuthMiddleware 的测试引擎。
// 受保护路由回显 user_id 和 role，用于断言上下文注入。
func newAuthEngine(manager *token.Manager) *route.Engine {
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	r.Use(AuthMiddleware(manager))
	r.GET("/protected", func(c context.Context, ctx *app.RequestContext) {
		ctx.String(http.StatusOK, fmt.Sprintf("uid=%d role=%s",
			ctx.GetInt64(CtxUserIDKey), ctx.GetString(CtxRoleKey)))
	})
	return r
}

// signToken 用测试密钥签出指定类型/主题的 token。
func signToken(t *testing.T, manager *token.Manager, subject string, expiresIn int64, typ string) string {
	t.Helper()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(expiresIn) * time.Second)),
		},
		Type: typ,
		Role: "USER",
	}
	s, err := manager.Sign(claims)
	require.NoError(t, err)
	return s
}

func performWithHeader(engine *route.Engine, value string) *ut.ResponseRecorder {
	if value == "" {
		return ut.PerformRequest(engine, http.MethodGet, "/protected", nil)
	}
	return ut.PerformRequest(engine, http.MethodGet, "/protected", nil,
		ut.Header{Key: "Authorization", Value: value})
}

func TestAuth_ValidAccessToken(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newAuthEngine(manager)
	tok := signToken(t, manager, "42", 3600, token.TypeAccess)

	w := performWithHeader(engine, "Bearer "+tok)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "uid=42 role=USER", w.Body.String())
}

func TestAuth_MissingHeader(t *testing.T) {
	engine := newAuthEngine(token.NewManager(testSecret))

	w := performWithHeader(engine, "")

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "MISSING_AUTH_HEADER")
	assert.NotContains(t, w.Body.String(), "uid=", "中间件拒绝后不得执行后续 handler")
}

func TestAuth_InvalidScheme(t *testing.T) {
	engine := newAuthEngine(token.NewManager(testSecret))

	w := performWithHeader(engine, "Basic abc123")

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_AUTH_TYPE")
	assert.NotContains(t, w.Body.String(), "uid=")
}

func TestAuth_MalformedHeader(t *testing.T) {
	engine := newAuthEngine(token.NewManager(testSecret))

	// 只有一个 token 片段，缺少 Bearer 前缀
	w := performWithHeader(engine, "justatoken")

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_AUTH_TYPE")
}

func TestAuth_RefreshTokenRejected(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newAuthEngine(manager)
	tok := signToken(t, manager, "42", 3600, token.TypeRefresh)

	w := performWithHeader(engine, "Bearer "+tok)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_TOKEN", "refresh token 不得充当 access token")
	assert.NotContains(t, w.Body.String(), "uid=")
}

func TestAuth_TamperedToken(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newAuthEngine(manager)
	tok := signToken(t, manager, "42", 3600, token.TypeAccess)

	tampered := tok[:len(tok)-1]
	if tok[len(tok)-1] == 'a' {
		tampered += "b"
	} else {
		tampered += "a"
	}
	w := performWithHeader(engine, "Bearer "+tampered)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_TOKEN")
}

func TestAuth_ExpiredToken(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newAuthEngine(manager)
	tok := signToken(t, manager, "42", -10, token.TypeAccess)

	w := performWithHeader(engine, "Bearer "+tok)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_TOKEN")
}

func TestAuth_NonIntegerSubject(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newAuthEngine(manager)
	tok := signToken(t, manager, "not-a-number", 3600, token.TypeAccess)

	w := performWithHeader(engine, "Bearer "+tok)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_TOKEN", "subject 非数字按无效 token 处理，而非 500")
}
