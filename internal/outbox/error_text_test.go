package outbox

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// 截断切在多字节字符中间时，结果必须仍是合法 UTF-8：PostgreSQL 会拒绝非法字节序列，
// last_error 写不进去，attempts 也不会递增，DLQ 上限随之失效。
func TestTruncateErrorKeepsValidUTF8(t *testing.T) {
	cases := map[string]string{
		"cut inside a rune": strings.Repeat("a", 499) + "中文",
		"all multibyte":     strings.Repeat("中", 200),
		"short text intact": "connection reset by peer",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := truncateError(in)
			require.LessOrEqual(t, len(got), maxErrorLength)
			require.True(t, utf8.ValidString(got), "truncated text must stay valid UTF-8")
			require.True(t, strings.HasPrefix(in, got), "truncated text must be a prefix of the input")
		})
	}
}
