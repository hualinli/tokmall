package feed

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestJitteredTTL_Range(t *testing.T) {
	base := 60 * time.Second
	for i := 0; i < 1000; i++ {
		got := jitteredTTL(base)
		require.GreaterOrEqual(t, got, base-jitterWindow, "TTL 不低于 base-window")
		require.LessOrEqual(t, got, base+jitterWindow, "TTL 不高于 base+window")
	}
}

func TestJitteredTTL_Varies(t *testing.T) {
	// 抖动的意义是错开过期时间：若所有 Set 用同一 TTL，仍然同时过期（雪崩未解）。
	base := 60 * time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 2000; i++ {
		seen[jitteredTTL(base)] = true
	}
	require.Greater(t, len(seen), 1, "抖动应产生多个不同 TTL，否则雪崩防护无效")
}
