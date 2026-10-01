// Package tracing 实现 W3C Trace Context（traceparent）的解析、生成与透传。
//
// 为什么自己实现而不是直接上 OpenTelemetry SDK：
//   - 本项目是单进程模块化单体，"分布式追踪"的收益主要来自**单请求内的
//     分段耗时 + 关联 ID**，而不是跨服务的跳数；
//   - traceparent 的格式是公开标准（version-traceid-spanid-flags），
//     解析与生成不到 100 行，零依赖、可单测；
//   - 关键收益（trace_id 进日志、能按 trace 串起一次请求）在阶段 1 就拿到了；
//     将来真拆服务时，把这里换成 OTel SDK + OTLP exporter 即可，
//     上下文传播的语义已经对齐（同样是 W3C TraceContext）。
//
// 与 request_id 的关系：request_id 是"一次 HTTP 请求"的标识（服务端生成、
// 客户端不可信）；trace_id 是"一条调用链"的标识（可跨进程传播）。
// 两者同时保留：前者用于运维对账，后者用于跨服务排障。
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/lihualin/tokmall/internal/observability"
)

const (
	// Header 是 W3C Trace Context 的标准头名。
	Header = "traceparent"
	// 版本 00 的固定长度：2 + 1 + 32 + 1 + 16 + 1 + 2 = 55 个字符。
	traceparentLength = 55
	version           = "00"
	// flagsSampled 表示调用方要求采样。本项目全量记录，仅原样透传该位。
	flagsSampled = "01"
)

// Span 是一次调用的追踪上下文。
type Span struct {
	TraceID string
	SpanID  string
	// ParentID 是上游传入的 span-id（没有上游时为空）。
	ParentID string
	Sampled  bool
}

// Parse 解析 traceparent 头。任何不符合规范的输入都返回 ok=false——
// 我们**不信任客户端传入的追踪头**：格式非法就丢弃并生成新的，
// 避免日志注入（把任意字符串塞进 trace_id 字段）与伪造调用链。
func Parse(header string) (Span, bool) {
	header = strings.TrimSpace(header)
	if len(header) != traceparentLength {
		return Span{}, false
	}
	parts := strings.Split(header, "-")
	if len(parts) != 4 {
		return Span{}, false
	}
	if parts[0] != version {
		// 只接受 00 版本；未来出现新版本时按规范应忽略未知字段，
		// 这里选择保守拒绝，并在测试里固定行为。
		return Span{}, false
	}
	if !isHex(parts[1], 32) || !isHex(parts[2], 16) || !isHex(parts[3], 2) {
		return Span{}, false
	}
	if allZero(parts[1]) || allZero(parts[2]) {
		// 规范明确禁止全 0 的 trace-id / span-id。
		return Span{}, false
	}
	return Span{
		TraceID:  strings.ToLower(parts[1]),
		ParentID: strings.ToLower(parts[2]),
		Sampled:  parts[3] == flagsSampled,
	}, true
}

// Start 生成一次调用的追踪上下文：沿用上游的 trace-id（如果有），
// 但总是生成新的 span-id——span-id 标识"当前这一跳"，不能复用上游的。
func Start(incoming string) Span {
	if parent, ok := Parse(incoming); ok {
		return Span{
			TraceID:  parent.TraceID,
			SpanID:   randomHex(8),
			ParentID: parent.ParentID,
			Sampled:  parent.Sampled,
		}
	}
	return Span{
		TraceID: randomHex(16),
		SpanID:  randomHex(8),
		Sampled: true,
	}
}

// Header 生成回写给下游/响应的 traceparent 值。
func (s Span) Header() string {
	flags := "00"
	if s.Sampled {
		flags = flagsSampled
	}
	return version + "-" + s.TraceID + "-" + s.SpanID + "-" + flags
}

// WithSpan 把 span 放进 context，后续日志自动带上 trace_id/span_id。
func WithSpan(ctx context.Context, span Span) context.Context {
	return observability.WithTrace(ctx, span.TraceID, span.SpanID)
}

func isHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'f':
		case char >= 'A' && char <= 'F':
		default:
			return false
		}
	}
	return true
}

func allZero(value string) bool {
	for _, char := range value {
		if char != '0' {
			return false
		}
	}
	return true
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败在实践中意味着系统熵源不可用；
		// 追踪 ID 退化成一个固定值也比 panic 好（它只影响可观测性）。
		return strings.Repeat("0", bytes*2-1) + "1"
	}
	return hex.EncodeToString(buf)
}
