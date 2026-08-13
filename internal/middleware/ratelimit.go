package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/hertz-contrib/requestid"
	"github.com/lihualin/tokmall/internal/response"
	"github.com/redis/go-redis/v9"
)

// Limiter 限流器消费侧接口：middleware 只依赖它，实现由 main 装配（Redis 版）。
type Limiter interface {
	// Allow 判断 key 在窗口内是否允许通过（已计入本次请求）。
	// 返回 error 表示限流依赖故障——由调用方决定降级策略（fail-open/fail-closed）。
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
}

// RateLimitRule 单条限流规则：限流维度由 Scope + ByUser 决定。
type RateLimitRule struct {
	Scope  string        // 逻辑分组（auth / write / read），进 key 与日志
	Limit  int           // 窗口内最大请求数
	Window time.Duration // 窗口时长（固定窗口，EXPIRE 即窗口）
	ByUser bool          // true=按 user_id 限（需已认证）；false=按 IP 限
}

// RateLimit 限流中间件。维度选择：
//   - ByUser=true 且已登录 → 按 user_id（写接口：每个用户自己的配额）
//   - ByUser=true 但匿名   → 回退按 IP（未登录用户共享 IP 配额）
//   - ByUser=false         → 一律按 IP（认证接口防暴力破解：未登录只能按 IP）
//
// 被限流 → 429 RATE_LIMITED（response 已定义映射）。
// 限流依赖故障（Redis 挂）→ fail-open 放行 + Warn：限流失效比接口不可用好，
// 认证类场景（login）若要 fail-closed（拒绝所有请求防暴力破解）需另行配置。
func RateLimit(l Limiter, rule RateLimitRule) app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		key := rateLimitKey(ctx, rule)
		allowed, err := l.Allow(c, key, rule.Limit, rule.Window)
		if err != nil {
			// fail-open：不阻塞请求，但必须留痕（限流防线失效本身是事故）
			slog.Warn("rate limiter unavailable, fail-open",
				"scope", rule.Scope, "key", key, "request_id", requestid.Get(ctx), "err", err)
			ctx.Next(c)
			return
		}
		if !allowed {
			response.Abort(ctx, response.ErrRateLimited)
			return
		}
		ctx.Next(c)
	}
}

// rateLimitKey 构造限流 key：ratelimit:{scope}:{dimension}:{id}。
// 固定窗口靠 EXPIRE 过期（窗口结束 key 消失，计数归零），key 本身不带时间戳。
func rateLimitKey(ctx *app.RequestContext, rule RateLimitRule) string {
	dimension, id := "ip", ctx.ClientIP()
	if rule.ByUser {
		if userID := ctx.GetInt64(CtxUserIDKey); userID > 0 {
			dimension, id = "user", strconv.FormatInt(userID, 10)
		}
	}
	return fmt.Sprintf("ratelimit:%s:%s:%s", rule.Scope, dimension, id)
}

// RedisLimiter 固定窗口计数：INCR + 首请求 EXPIRE，lua 原子执行。
// 窗口边界有毛刺（59s 打满 + 1s 打满 = 两倍突发），对学习项目可接受；
// 需要平滑时换令牌桶（lua 多 token 操作）。
type RedisLimiter struct {
	client *redis.Client
	// incrScript 原子 INCR 并为首个请求设置窗口过期：
	// 计数=1 时设置 TTL，之后只 INCR；窗口结束 key 自动消失，重新从 1 计。
	incrScript *redis.Script
}

func NewRedisLimiter(client *redis.Client) *RedisLimiter {
	return &RedisLimiter{
		client: client,
		incrScript: redis.NewScript(`
local c = redis.call('INCR', KEYS[1])
if c == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return c`),
	}
}

// Allow 返回窗口内是否允许（含本次）。Redis 故障 → error（调用方 fail-open）。
func (r *RedisLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	count, err := r.incrScript.Run(ctx, r.client, []string{key}, int64(window.Seconds())).Int()
	if err != nil {
		return false, fmt.Errorf("rate limit incr: %w", err)
	}
	return count <= limit, nil
}
