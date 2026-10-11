package user

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	hzconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/token"
)

// 账号配额只挂在携带标识符的登录与注册上；refresh/logout 只经过 common（IP）链。
// 这是 NAT/CGNAT 误伤的修复点：没有标识符的请求不能回退成“按 IP 计 10/min”。
func TestRegisterRoutesAttachesCredentialLimitOnlyToLoginAndRegister(t *testing.T) {
	var seen []string
	mark := func(name string) app.HandlerFunc {
		return func(ctx context.Context, c *app.RequestContext) {
			seen = append(seen, name)
			c.Next(ctx)
		}
	}
	// 真实处理器跑在 fake 仓储上：空输入只会得到 4xx，不会 panic。
	svc := NewService(newFakeRepo(), token.NewManager([]byte("routes-test-secret")), newFakeBlacklist(), nil, nil)

	cases := map[string][]string{
		"/api/v1/auth/login":    {"common", "credential"},
		"/api/v1/auth/register": {"common", "credential"},
		"/api/v1/auth/refresh":  {"common"},
		"/api/v1/auth/logout":   {"common"},
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			seen = nil
			engine := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
			NewHandler(svc).RegisterRoutes(engine.Group("/api/v1"),
				[]app.HandlerFunc{mark("common")}, []app.HandlerFunc{mark("credential")})
			body := `{}`
			w := ut.PerformRequest(engine, http.MethodPost, path,
				&ut.Body{Body: strings.NewReader(body), Len: len(body)},
				ut.Header{Key: "Content-Type", Value: "application/json"})
			require.Equal(t, want, seen)
			require.Less(t, w.Code, http.StatusInternalServerError, "handler must run against the fake service")
		})
	}
}
