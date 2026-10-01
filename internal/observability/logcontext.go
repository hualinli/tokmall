package observability

import (
	"context"
	"log/slog"
)

// 这一层解决的是"日志能不能串起来"的问题。
//
// 现状（改造前）：request_id 只在两处手写进日志，其余日志没有关联 ID；
// 一次请求跨 5 个模块打了 20 条日志，却没有任何字段能把它们归到同一次请求。
//
// 改造后：所有日志经过 ContextHandler，它从 context 里取出本次请求的
// request_id / trace_id / span_id / route 自动附加，业务代码只要
// slog.InfoContext(ctx, ...) 就自动带上——不依赖任何人的自觉。
//
// 另一个职责是脱敏：字段名命中黑名单时把值替换成 ***，避免"哪天有人
// 手滑 slog.Info("login", "password", p)" 把口令写进日志。
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyTraceID
	ctxKeySpanID
	ctxKeyRoute
	ctxKeyUserID
)

// WithRequestID 把 requestid 中间件生成的 ID 放进 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// WithTrace 放入 W3C traceparent 解析/生成的 trace 上下文。
func WithTrace(ctx context.Context, traceID, spanID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyTraceID, traceID)
	return context.WithValue(ctx, ctxKeySpanID, spanID)
}

// WithRoute 放入路由模板（不是原始 path，避免高基数）。
func WithRoute(ctx context.Context, route string) context.Context {
	return context.WithValue(ctx, ctxKeyRoute, route)
}

// WithUserID 放入已认证用户（只进日志，绝不进指标标签）。
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, userID)
}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(ctxKeyRequestID).(string)
	return value
}

func TraceID(ctx context.Context) string {
	value, _ := ctx.Value(ctxKeyTraceID).(string)
	return value
}

func SpanID(ctx context.Context) string {
	value, _ := ctx.Value(ctxKeySpanID).(string)
	return value
}

// sensitiveKeys 是日志字段的脱敏黑名单。命中即替换为 "***"。
// 只按字段名判断，不做值的内容嗅探：后者误伤率高且成本大。
var sensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "pwd": true,
	"token": true, "access_token": true, "refresh_token": true,
	"authorization": true, "cookie": true, "secret": true, "jwt": true,
	"password_hash": true, "api_key": true, "apikey": true,
}

const redacted = "***"

// ContextHandler 是一个 slog.Handler 装饰器：
//   - 自动注入请求级字段（request_id/trace_id/span_id/route/user_id）
//   - 对敏感字段脱敏
type ContextHandler struct {
	next slog.Handler
}

func NewContextHandler(next slog.Handler) *ContextHandler {
	return &ContextHandler{next: next}
}

func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *ContextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := RequestID(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	if id := TraceID(ctx); id != "" {
		record.AddAttrs(slog.String("trace_id", id))
	}
	if id := SpanID(ctx); id != "" {
		record.AddAttrs(slog.String("span_id", id))
	}
	if route := Route(ctx); route != "" {
		record.AddAttrs(slog.String("route", route))
	}
	if userID, ok := ctx.Value(ctxKeyUserID).(int64); ok && userID > 0 {
		record.AddAttrs(slog.Int64("user_id", userID))
	}
	return h.next.Handle(ctx, redactRecord(record))
}

func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{next: h.next.WithAttrs(redactAttrs(attrs))}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{next: h.next.WithGroup(name)}
}

// Route 从 context 取路由模板。
func Route(ctx context.Context) string {
	value, _ := ctx.Value(ctxKeyRoute).(string)
	return value
}

func redactRecord(record slog.Record) slog.Record {
	redactedRecord := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		redactedRecord.AddAttrs(redactAttr(attr))
		return true
	})
	return redactedRecord
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, redactAttr(attr))
	}
	return out
}

func redactAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if sensitiveKeys[attr.Key] {
		return slog.String(attr.Key, redacted)
	}
	// 组内字段同样要脱敏：slog.Group("auth", slog.String("password", p)) 很常见。
	if attr.Value.Kind() == slog.KindGroup {
		return slog.Group(attr.Key, attrsToAny(redactAttrs(attr.Value.Group()))...)
	}
	return attr
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, attr)
	}
	return out
}
