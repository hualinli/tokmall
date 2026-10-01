package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type recordingSlowStats struct {
	commands []string
}

func (s *recordingSlowStats) RedisSlowCommand(command string) {
	s.commands = append(s.commands, command)
}

// slowHook 用可控的 next 函数模拟"命令耗时"，避免测试依赖真实 Redis 的延迟。
func slowHook(t *testing.T, elapsed time.Duration, stats SlowCommandStats) redis.ProcessHook {
	t.Helper()
	hook := NewSlowCommandHook(time.Millisecond, stats)
	return hook.ProcessHook(func(context.Context, redis.Cmder) error {
		time.Sleep(elapsed)
		return nil
	})
}

func TestSlowCommandHookIgnoresFastCommands(t *testing.T) {
	stats := &recordingSlowStats{}
	hook := slowHook(t, 0, stats)

	cmd := redis.NewStringCmd(context.Background(), "get", "feed:first_page")
	require.NoError(t, hook(context.Background(), cmd))
	require.Empty(t, stats.commands, "快命令不应被记录")
}

func TestSlowCommandHookRecordsSlowCommands(t *testing.T) {
	stats := &recordingSlowStats{}
	hook := slowHook(t, 5*time.Millisecond, stats)

	cmd := redis.NewStringCmd(context.Background(), "hgetall", "user_friends:12345")
	require.NoError(t, hook(context.Background(), cmd))
	require.Equal(t, []string{"hgetall"}, stats.commands, "慢命令按命令名计数")
}

// TestKeyPrefixHidesIdentifiers：日志里只保留 key 前缀——
// 完整 key 常含用户 ID/订单号（高基数 + 隐私）。
func TestKeyPrefixHidesIdentifiers(t *testing.T) {
	cases := map[string]string{
		"feed:following:full-v1:12345": "feed:following:full-v1",
		"existence:missing:video:999":  "existence:missing:video",
		"feed:first_page":              "feed:first_page",
		"":                             "",
		"12345":                        "12345",
	}
	for key, want := range cases {
		require.Equalf(t, want, keyPrefixOf(key), "key %q", key)
	}
}

// TestSlowPipelineRecorded：管道整体慢时记一条（含命令列表与大小），
// 而不是把管道里的每条命令各记一条（那会把日志刷爆）。
func TestSlowPipelineRecorded(t *testing.T) {
	stats := &recordingSlowStats{}
	hook := NewSlowCommandHook(time.Millisecond, stats)
	pipelineHook := hook.ProcessPipelineHook(func(context.Context, []redis.Cmder) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	})

	cmds := []redis.Cmder{
		redis.NewStringCmd(context.Background(), "get", "a"),
		redis.NewStringCmd(context.Background(), "set", "b", "1"),
	}
	require.NoError(t, pipelineHook(context.Background(), cmds))
	require.Equal(t, []string{"pipeline"}, stats.commands)
}

// TestBlockingCommandsAreNotReported：阻塞命令"慢"是设计如此。
// 实测踩到过：Outbox 消费者的 XREADGROUP ... BLOCK 让慢命令计数一直涨，
// 看起来像 Redis 变慢，其实是消费者在正常等待。
func TestBlockingCommandsAreNotReported(t *testing.T) {
	stats := &recordingSlowStats{}
	hook := slowHook(t, 5*time.Millisecond, stats)

	for _, command := range []string{"xreadgroup", "blpop", "brpop", "subscribe"} {
		cmd := redis.NewStringCmd(context.Background(), command, "args")
		require.NoError(t, hook(context.Background(), cmd))
	}
	require.Empty(t, stats.commands, "阻塞命令不应计入慢命令")
}

func TestSlowCommandHookPassesThroughErrors(t *testing.T) {
	stats := &recordingSlowStats{}
	hook := NewSlowCommandHook(time.Millisecond, stats)
	expected := errors.New("connection refused")
	process := hook.ProcessHook(func(context.Context, redis.Cmder) error {
		time.Sleep(2 * time.Millisecond)
		return expected
	})

	cmd := redis.NewStringCmd(context.Background(), "get", "feed:first_page")
	require.ErrorIs(t, process(context.Background(), cmd), expected, "Hook 不得吞掉错误")
	require.Equal(t, []string{"get"}, stats.commands)
}
