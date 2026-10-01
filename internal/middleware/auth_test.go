package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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

	// 篡改签名**中间**的字符，而不是最后一个：
	// base64url 的最后一个字符可能带非规范尾部位，换成另一个字符仍会解码出
	// 相同的字节，签名因此没变——这个用例曾经因此偶发失败（实测 flaky）。
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)
	signature := []byte(parts[2])
	index := len(signature) / 2
	if signature[index] == 'A' {
		signature[index] = 'B'
	} else {
		signature[index] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(signature)
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

// newOptionalEngine 构造挂了 AuthOptional 的测试引擎：回显 viewer_id，0 表示匿名。
func newOptionalEngine(manager *token.Manager) *route.Engine {
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	r.Use(AuthOptional(manager))
	r.GET("/public", func(c context.Context, ctx *app.RequestContext) {
		ctx.String(http.StatusOK, fmt.Sprintf("uid=%d", ctx.GetInt64(CtxUserIDKey)))
	})
	return r
}

func performOptional(engine *route.Engine, value string) *ut.ResponseRecorder {
	if value == "" {
		return ut.PerformRequest(engine, http.MethodGet, "/public", nil)
	}
	return ut.PerformRequest(engine, http.MethodGet, "/public", nil,
		ut.Header{Key: "Authorization", Value: value})
}

func TestAuthOptional_Anonymous(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newOptionalEngine(manager)

	w := performOptional(engine, "")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "uid=0", w.Body.String(), "无 header 必须匿名（uid=0）")
}

func TestAuthOptional_ValidToken(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newOptionalEngine(manager)

	w := performOptional(engine, "Bearer "+signToken(t, manager, "7", 900, token.TypeAccess))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "uid=7", w.Body.String())
}

func TestAuthOptional_InvalidTokenIs401(t *testing.T) {
	manager := token.NewManager(testSecret)
	engine := newOptionalEngine(manager)

	// 有 header 但无效：绝不静默降级为匿名
	w := performOptional(engine, "Bearer forged.token.value")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
