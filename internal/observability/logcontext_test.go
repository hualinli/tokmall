package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// newCapturingLogger 返回一个把日志写进 buffer 的 logger，便于断言字段。
func newCapturingLogger(buf *bytes.Buffer) *slog.Logger {
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(NewContextHandler(base))
}

// TestContextHandlerInjectsRequestScopedFields 是这一层存在的理由：
// 业务代码只写 slog.InfoContext(ctx, "msg")，请求级字段必须自动出现。
func TestContextHandlerInjectsRequestScopedFields(t *testing.T) {
	var buf bytes.Buffer
	logger := newCapturingLogger(&buf)

	ctx := WithRequestID(context.Background(), "req-1")
	ctx = WithTrace(ctx, "trace-1", "span-1")
	ctx = WithRoute(ctx, "/api/v1/orders/:id")
	ctx = WithUserID(ctx, 42)
	logger.InfoContext(ctx, "order created")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	require.Equal(t, "req-1", record["request_id"])
	require.Equal(t, "trace-1", record["trace_id"])
	require.Equal(t, "span-1", record["span_id"])
	require.Equal(t, "/api/v1/orders/:id", record["route"])
	require.Equal(t, float64(42), record["user_id"])
}

// TestContextHandlerWithoutContextStaysClean：没有请求上下文时不应凭空造字段。
func TestContextHandlerWithoutContextStaysClean(t *testing.T) {
	var buf bytes.Buffer
	logger := newCapturingLogger(&buf)
	logger.Info("startup complete")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	require.NotContains(t, record, "request_id")
	require.NotContains(t, record, "trace_id")
	require.NotContains(t, record, "user_id")
}

// TestContextHandlerRedactsSensitiveFields：脱敏是兜底，防止"手滑把口令写进日志"。
func TestContextHandlerRedactsSensitiveFields(t *testing.T) {
	var buf bytes.Buffer
	logger := newCapturingLogger(&buf)
	logger.Info("login attempt",
		"username", "alice",
		"password", "s3cr3t",
		"access_token", "eyJhbGciOi",
		"nested", slog.GroupValue(slog.String("refresh_token", "abc"), slog.String("ip", "1.2.3.4")),
	)

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	require.Equal(t, "alice", record["username"], "非敏感字段保持原值")
	require.Equal(t, redacted, record["password"])
	require.Equal(t, redacted, record["access_token"])

	nested, ok := record["nested"].(map[string]any)
	require.True(t, ok, "group 字段应保留结构")
	require.Equal(t, redacted, nested["refresh_token"], "组内字段同样要脱敏")
	require.Equal(t, "1.2.3.4", nested["ip"])
}

// TestContextHandlerRedactsWithAttrs：WithAttrs 是"预绑定字段"的路径，
// 也必须走脱敏，否则 slog.With("password", p) 会绕过检查。
func TestContextHandlerRedactsWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := newCapturingLogger(&buf).With("password", "leaked")
	logger.Info("boom")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	require.Equal(t, redacted, record["password"])
}
