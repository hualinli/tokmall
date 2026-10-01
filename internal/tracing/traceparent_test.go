package tracing

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAcceptsValidTraceparent(t *testing.T) {
	header := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	span, ok := Parse(header)
	require.True(t, ok)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.TraceID)
	require.Equal(t, "00f067aa0ba902b7", span.ParentID)
	require.True(t, span.Sampled)
}

// TestParseRejectsMalformed：追踪头来自客户端，必须严格校验。
// 放行任意字符串等于让攻击者往日志的 trace_id 字段里注入内容。
func TestParseRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"too short":          "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
		"wrong version":      "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"trace id too short": "00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01",
		"non hex":            "00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01",
		"all zero trace id":  "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"all zero span id":   "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"injection attempt":  "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01 x=1",
		"missing flags":      "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-",
		"extra segment":      "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok := Parse(header)
			require.Falsef(t, ok, "非法 traceparent 必须被拒绝：%q", header)
		})
	}
}

// TestStartKeepsUpstreamTraceIDButAlwaysNewSpanID：
// 同一条链要共用一个 trace-id（否则串不起来），但每一跳必须是新的 span-id。
func TestStartKeepsUpstreamTraceIDButAlwaysNewSpanID(t *testing.T) {
	upstream := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	span := Start(upstream)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.TraceID)
	require.NotEqual(t, "00f067aa0ba902b7", span.SpanID)
	require.Equal(t, "00f067aa0ba902b7", span.ParentID, "上游 span-id 作为 parent 保留")
	require.Len(t, span.SpanID, 16)
}

// TestStartGeneratesFreshTraceWhenHeaderInvalid：没有上游（或上游非法）时自己起一条链。
func TestStartGeneratesFreshTraceWhenHeaderInvalid(t *testing.T) {
	first := Start("")
	second := Start("garbage")
	require.Len(t, first.TraceID, 32)
	require.Len(t, first.SpanID, 16)
	require.NotEqual(t, first.TraceID, second.TraceID, "两次独立请求必须是不同的链")
	require.True(t, first.Sampled)
}

// TestHeaderRoundTrip：写回给下游/响应的值必须能被再次解析（自洽）。
func TestHeaderRoundTrip(t *testing.T) {
	span := Start("")
	header := span.Header()
	require.Len(t, header, traceparentLength)

	parsed, ok := Parse(header)
	require.True(t, ok)
	require.Equal(t, span.TraceID, parsed.TraceID)
	require.Equal(t, span.SpanID, parsed.ParentID)
}

func TestHeaderLowercasesHex(t *testing.T) {
	span, ok := Parse("00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-01")
	require.True(t, ok)
	require.Equal(t, strings.ToLower("4BF92F3577B34DA6A3CE929D0E0E4736"), span.TraceID)
	require.False(t, strings.ContainsAny(span.Header(), "ABCDEF"))
}
