package cache

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SlowCommandThreshold 是 Redis 慢命令的判定阈值。
//
// 为什么需要它：缓存命中后系统的瓶颈就从数据库移到了 Redis（见
// docs/performance-baseline.md），但在此之前 Redis 是"黑盒"——只有命中率指标，
// 看不到"哪条命令慢、慢多少、打在什么 key 上"。慢命令日志把这一层打开。
//
// 阈值取 10ms：正常命令在本地 Redis 上应当 <1ms，超过 10ms 说明要么网络抖动、
// 要么命中了 O(N) 命令（KEYS/HGETALL 大 Hash/SMEMBERS 大集合）——后者才是真问题。
const SlowCommandThreshold = 10 * time.Millisecond

// SlowCommandHook 是 go-redis 的 Hook：记录慢命令到统一结构化日志。
//
// 只打**命令名 + 耗时 + key 前缀**，不打完整 key：
//   - 完整 key 常含用户 ID/订单号（高基数 + 隐私），进日志后既难聚合也不合规；
//   - 排查时"哪类 key 慢"比"具体哪个 key 慢"更有用（后者通常是个例）。
type SlowCommandHook struct {
	threshold time.Duration
	logger    *slog.Logger
	stats     SlowCommandStats
}

// SlowCommandStats 是慢命令计数器（由 observability 实现）。
type SlowCommandStats interface {
	RedisSlowCommand(command string)
}

type nopSlowStats struct{}

func (nopSlowStats) RedisSlowCommand(string) {}

func NewSlowCommandHook(threshold time.Duration, stats SlowCommandStats) *SlowCommandHook {
	if threshold <= 0 {
		threshold = SlowCommandThreshold
	}
	if stats == nil {
		stats = nopSlowStats{}
	}
	return &SlowCommandHook{threshold: threshold, logger: slog.Default(), stats: stats}
}

func (h *SlowCommandHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *SlowCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		started := time.Now()
		err := next(ctx, cmd)
		h.observe(ctx, cmd, time.Since(started), err)
		return err
	}
}

func (h *SlowCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		started := time.Now()
		err := next(ctx, cmds)
		elapsed := time.Since(started)
		if elapsed < h.threshold {
			return err
		}
		names := make([]string, 0, len(cmds))
		for _, cmd := range cmds {
			names = append(names, cmd.Name())
		}
		h.report(ctx, strings.Join(names, ","), len(cmds), elapsed, err)
		return err
	}
}

// blockingCommands 是**故意阻塞**的命令：它们"慢"是设计如此，
// 计入慢命令只会制造噪音。
//
// 实测踩到过：Outbox 消费者用 `XREADGROUP ... BLOCK 5000` 阻塞等消息，
// 于是 `redis_slow_commands_total{command="xreadgroup"}` 一直在涨，
// 看起来像 Redis 变慢了，其实是消费者在正常地等待。
var blockingCommands = map[string]bool{
	"xread": true, "xreadgroup": true,
	"blpop": true, "brpop": true, "brpoplpush": true, "blmove": true, "blmpop": true,
	"bzpopmin": true, "bzpopmax": true, "bzmpop": true,
	"subscribe": true, "psubscribe": true, "ssubscribe": true,
	"wait": true, "waitaof": true,
}

func (h *SlowCommandHook) observe(ctx context.Context, cmd redis.Cmder, elapsed time.Duration, err error) {
	if elapsed < h.threshold {
		return
	}
	command := cmd.Name()
	if blockingCommands[command] {
		return
	}
	// 慢命令按命令名计数：既能看趋势（哪类命令在变慢），又不会因为 key 不同而爆炸。
	h.stats.RedisSlowCommand(command)

	args := cmd.Args()
	keyPrefix := ""
	if len(args) > 1 {
		if key, ok := args[1].(string); ok {
			keyPrefix = keyPrefixOf(key)
		}
	}
	attrs := []any{
		"redis_command", command,
		"redis_duration_ms", float64(elapsed.Microseconds()) / 1000.0,
		"redis_key_prefix", keyPrefix,
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		attrs = append(attrs, "err", err.Error())
		h.logger.WarnContext(ctx, "redis slow command", attrs...)
		return
	}
	h.logger.WarnContext(ctx, "redis slow command", attrs...)
}

func (h *SlowCommandHook) report(ctx context.Context, commands string, count int, elapsed time.Duration, err error) {
	h.stats.RedisSlowCommand("pipeline")
	attrs := []any{
		"redis_command", commands,
		"redis_pipeline_size", count,
		"redis_duration_ms", float64(elapsed.Microseconds()) / 1000.0,
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		attrs = append(attrs, "err", err.Error())
	}
	h.logger.WarnContext(ctx, "redis slow pipeline", attrs...)
}

// keyPrefixOf 把 key 截断成"前缀"：保留第一段（业务域），丢掉后面的标识符。
// feed:following:full-v1:12345 → feed:following:full-v1
func keyPrefixOf(key string) string {
	if key == "" {
		return ""
	}
	parts := strings.Split(key, ":")
	// 末段通常是 ID；若末段全是数字则丢掉它。
	last := parts[len(parts)-1]
	if isNumeric(last) && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	prefix := strings.Join(parts, ":")
	if len(prefix) > 64 {
		prefix = prefix[:64]
	}
	return prefix
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
