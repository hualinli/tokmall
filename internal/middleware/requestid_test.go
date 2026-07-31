package middleware

import (
	"context"
	"net/http"
	"regexp"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	hzconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/google/uuid"
	"github.com/hertz-contrib/requestid"
	"github.com/stretchr/testify/assert"
)

// newTestEngine 构造一个挂了 requestid 中间件的测试引擎。
// 复刻 main.go 中的生产配置：WithGenerator 生成 UUID v7。
// route.NewEngine 起引擎（不监听端口，ut 走内存链路）。
func newTestEngine(opts ...requestid.Option) *route.Engine {
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	generator := requestid.WithGenerator(func(ctx context.Context, c *app.RequestContext) string {
		return uuid.Must(uuid.NewV7()).String()
	})
	all := append([]requestid.Option{generator}, opts...)
	r.Use(requestid.New(all...))
	r.GET("/echo", func(ctx context.Context, c *app.RequestContext) {
		c.String(http.StatusOK, requestid.Get(c))
	})
	return r
}

func TestRequestID_GeneratedWhenMissing(t *testing.T) {
	engine := newTestEngine()

	w := ut.PerformRequest(engine, http.MethodGet, "/echo", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	got := w.Header().Get("X-Request-ID")
	assert.NotEmpty(t, got, "缺失请求头时服务端应生成 request id")

	// v7 UUID 的特征：第 3 组开头是 7，且整体是标准 UUID 格式
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	assert.Regexp(t, re, got)
}

func TestRequestID_PassthroughWhenProvided(t *testing.T) {
	engine := newTestEngine()

	w := ut.PerformRequest(engine, http.MethodGet, "/echo", nil,
		ut.Header{Key: "X-Request-ID", Value: "my-custom-id"},
	)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "my-custom-id", w.Header().Get("X-Request-ID"),
		"带请求头时应原样透传，而不是重新生成")
}

func TestRequestID_CustomGenerator(t *testing.T) {
	engine := newTestEngine(
		requestid.WithGenerator(func(ctx context.Context, c *app.RequestContext) string {
			return "fixed-test-id"
		}),
	)

	w := ut.PerformRequest(engine, http.MethodGet, "/echo", nil)

	assert.Equal(t, "fixed-test-id", w.Header().Get("X-Request-ID"),
		"自定义 generator 应覆盖默认生成逻辑")
}
