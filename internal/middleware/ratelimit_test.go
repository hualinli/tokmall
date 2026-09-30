package middleware

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hzconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/stretchr/testify/require"
)

type fakeLimiter struct {
	allowed bool
	err     error
	lastKey string
}

func (f *fakeLimiter) Allow(_ context.Context, key string, _ int, _ time.Duration) (bool, error) {
	f.lastKey = key
	return f.allowed, f.err
}

// newRateLimitEngine 构造挂了限流中间件的测试引擎（ut 内存链路）。
func newRateLimitEngine(l Limiter, rule RateLimitRule) *route.Engine {
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	r.Use(RateLimit(l, rule))
	r.GET("/echo", func(ctx context.Context, c *app.RequestContext) {
		c.String(http.StatusOK, "ok")
	})
	return r
}

func TestRateLimit_AllowsWithinLimit(t *testing.T) {
	limiter := &fakeLimiter{allowed: true}
	engine := newRateLimitEngine(limiter, RateLimitRule{Scope: "read", Limit: 100, Window: 10 * time.Second})

	w := ut.PerformRequest(engine, http.MethodGet, "/echo", nil)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRateLimit_RejectsOverLimit(t *testing.T) {
	limiter := &fakeLimiter{allowed: false}
	engine := newRateLimitEngine(limiter, RateLimitRule{Scope: "write", Limit: 100, Window: 10 * time.Second})

	w := ut.PerformRequest(engine, http.MethodGet, "/echo", nil)
	require.Equal(t, http.StatusTooManyRequests, w.Code, "超限应返回 429")
	require.Contains(t, w.Body.String(), "RATE_LIMITED")
}

func TestRateLimit_FailOpenOnLimiterError(t *testing.T) {
	// 限流依赖故障（Redis 挂）→ fail-open：放行但留痕，不阻塞请求
	limiter := &fakeLimiter{err: errRateLimitDown}
	engine := newRateLimitEngine(limiter, RateLimitRule{Scope: "read", Limit: 100, Window: 10 * time.Second})

	w := ut.PerformRequest(engine, http.MethodGet, "/echo", nil)
	require.Equal(t, http.StatusOK, w.Code, "limiter 故障应 fail-open 放行")
}

var errRateLimitDown = &rateLimitDownError{}

type rateLimitDownError struct{}

func (*rateLimitDownError) Error() string { return "redis down" }

func TestRateLimitKey_ByUserLoggedIn(t *testing.T) {
	limiter := &fakeLimiter{allowed: true}
	engine := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	// 先挂一个注入 user_id 的中间件模拟认证，再挂限流
	engine.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Set(CtxUserIDKey, int64(42))
		c.Next(ctx)
	})
	engine.Use(RateLimit(limiter, RateLimitRule{Scope: "write", Limit: 100, Window: 10 * time.Second, ByUser: true}))
	engine.GET("/echo", func(ctx context.Context, c *app.RequestContext) {
		c.String(http.StatusOK, "ok")
	})

	ut.PerformRequest(engine, http.MethodGet, "/echo", nil)
	require.Contains(t, limiter.lastKey, "user:42", "登录用户按 user_id 限流")
}

func TestRateLimitKey_ByUserAnonymousFallsBackToIP(t *testing.T) {
	limiter := &fakeLimiter{allowed: true}
	engine := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	engine.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Set(CtxUserIDKey, int64(0)) // 匿名
		c.Next(ctx)
	})
	engine.Use(RateLimit(limiter, RateLimitRule{Scope: "write", Limit: 100, Window: 10 * time.Second, ByUser: true}))
	engine.GET("/echo", func(ctx context.Context, c *app.RequestContext) {
		c.String(http.StatusOK, "ok")
	})

	ut.PerformRequest(engine, http.MethodGet, "/echo", nil)
	require.Contains(t, limiter.lastKey, "ratelimit:write:ip:", "匿名回退按 IP 限流")
}

func TestAccountKeyExtractor_LoginIdentifier(t *testing.T) {
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	r.POST("/login", func(ctx context.Context, c *app.RequestContext) {
		dim, id, ok := AccountKeyExtractor(c)
		require.True(t, ok)
		require.Equal(t, "account", dim)
		require.Equal(t, "alice31", id)
		c.String(http.StatusOK, "ok")
	})
	ut.PerformRequest(r, http.MethodPost, "/login",
		&ut.Body{Body: bytes.NewBufferString(`{"identifier":"alice31","password":"x"}`), Len: 39},
		ut.Header{Key: "Content-Type", Value: "application/json"})
}

func TestAccountKeyExtractor_RegisterEmailFallback(t *testing.T) {
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	r.POST("/register", func(ctx context.Context, c *app.RequestContext) {
		_, id, ok := AccountKeyExtractor(c)
		require.True(t, ok)
		require.Equal(t, "bob@test.dev", id)
		c.String(http.StatusOK, "ok")
	})
	ut.PerformRequest(r, http.MethodPost, "/register",
		&ut.Body{Body: bytes.NewBufferString(`{"email":"bob@test.dev","username":"bob","password":"x"}`), Len: 56},
		ut.Header{Key: "Content-Type", Value: "application/json"})
}

func TestAccountKeyExtractor_NoIdentifierFallsBack(t *testing.T) {
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	r.POST("/refresh", func(ctx context.Context, c *app.RequestContext) {
		_, _, ok := AccountKeyExtractor(c)
		require.False(t, ok, "refresh 无账号标识 → 回退 IP")
		c.String(http.StatusOK, "ok")
	})
	ut.PerformRequest(r, http.MethodPost, "/refresh",
		&ut.Body{Body: bytes.NewBufferString(`{"refresh_token":"x"}`), Len: 21},
		ut.Header{Key: "Content-Type", Value: "application/json"})
}

// TestAccountKeyExtractor_NormalizesIdentifier 锁住审计结论：
// 限流 key 必须与业务查询用同一套规范化，否则给登录名加个空格就能换一个
// 限流桶，10/min/账号的防爆破形同虚设。
func TestAccountKeyExtractor_NormalizesIdentifier(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "trim and lowercase", body: `{"identifier":"  Alice@Example.COM "}`, want: "alice@example.com"},
		{name: "username lowercase", body: `{"username":"Bob123"}`, want: "bob123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
			r.POST("/login", func(ctx context.Context, c *app.RequestContext) {
				_, id, ok := AccountKeyExtractor(c)
				require.True(t, ok)
				require.Equal(t, tc.want, id)
				c.String(http.StatusOK, "ok")
			})
			ut.PerformRequest(r, http.MethodPost, "/login",
				&ut.Body{Body: bytes.NewBufferString(tc.body), Len: len(tc.body)},
				ut.Header{Key: "Content-Type", Value: "application/json"})
		})
	}
}

func TestNormalizeAccountKey(t *testing.T) {
	require.Equal(t, "alice", NormalizeAccountKey("  ALICE "))
	require.Empty(t, NormalizeAccountKey("   "))
}
