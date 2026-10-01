package middleware

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/hertz-contrib/requestid"
	"github.com/lihualin/tokmall/internal/observability"
	"github.com/lihualin/tokmall/internal/tracing"
)

// Metrics 是访问指标的最小接口（由 observability.HTTPMetrics 实现）。
type Metrics interface {
	Observe(method, route string, status int, seconds float64)
	IncInFlight()
	DecInFlight()
}

// HTTPMetrics 中间件：记录 route 模板化的耗时直方图与并发数。
// 与 AccessLog 分开：日志给人和 grep 用，指标给看板和告警用，
// 两者的采样/保留策略不同，不应该耦合在一个中间件里。
func HTTPMetrics(metrics Metrics) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		started := time.Now()
		metrics.IncInFlight()
		c.Next(ctx)
		metrics.DecInFlight()

		route := c.FullPath()
		metrics.Observe(string(c.Method()), route, c.Response.StatusCode(), time.Since(started).Seconds())
	}
}

// Trace 中间件：解析 W3C traceparent、生成当前 span，并放进 context。
//
// 三个动作：
//  1. 读 traceparent：合法就沿用上游 trace-id（保证跨服务同一条链），
//     非法就丢弃（不信任客户端输入）；
//  2. 生成新的 span-id 并写回响应头，方便前端/网关把一次调用串起来；
//  3. 把 request_id / trace_id / span_id 一起塞进 context，
//     之后任何 slog.InfoContext(ctx, ...) 都会自动带上（见 observability.ContextHandler）。
func Trace() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		span := tracing.Start(string(c.Request.Header.Get(tracing.Header)))
		c.Response.Header.Set(tracing.Header, span.Header())
		c.Set("trace_id", span.TraceID)

		requestCtx := tracing.WithSpan(ctx, span)
		if rid := requestid.Get(c); rid != "" {
			requestCtx = observability.WithRequestID(requestCtx, rid)
		}
		c.Next(requestCtx)
	}
}

// AccessLog 中间件：每个请求结束后打**一条**结构化访问日志。
//
// 为什么要有它：业务日志回答"发生了什么"，访问日志回答"这次请求怎么样"。
// 有了它，排查一个 500 的第一步是 grep trace_id，而不是在几十条日志里翻。
//
// route 用 c.FullPath() 的路由模板（/api/v1/videos/:id）而不是原始 path：
// 后者会让 /videos/1、/videos/2 …… 变成无限多个日志/指标维度。
func AccessLog() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		started := time.Now()
		c.Next(ctx)

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		// 让后续日志（包括 handler 里打的）都带上路由模板。
		logCtx := observability.WithRoute(ctx, route)
		if userID := c.GetInt64(CtxUserIDKey); userID > 0 {
			logCtx = observability.WithUserID(logCtx, userID)
		}

		status := c.Response.StatusCode()
		attrs := []any{
			"method", string(c.Method()),
			"route", route,
			"status", status,
			"duration_ms", float64(time.Since(started).Microseconds()) / 1000.0,
			"bytes", len(c.Response.Body()),
			"client_ip", c.ClientIP(),
		}
		switch {
		case status >= 500:
			slog.ErrorContext(logCtx, "http_request", attrs...)
		case status >= 400:
			// 4xx 是客户端问题：Warn 级别，不打 Error，避免污染错误率告警。
			slog.WarnContext(logCtx, "http_request", attrs...)
		default:
			slog.InfoContext(logCtx, "http_request", attrs...)
		}
	}
}
