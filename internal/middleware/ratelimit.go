package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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

// KeyExtractor 从请求中提取限流维度：用于按账号限流（auth 场景，
// 暴力破解的维度是账号而非 IP——NAT 下多人共享 IP，按 IP 会误伤真实用户）。
// 返回 (dimension, id, ok)；ok=false 时回退规则默认维度。
type KeyExtractor func(c *app.RequestContext) (dimension, id string, ok bool)

// AccountKeyExtractor 解析 auth 请求体中的账号标识（login: identifier；
// register: email/username），作为限流 key。refresh/logout 无标识 → 回退 IP。
// 注意：hertz 的 Body() 是 lazy 缓存，中间件读取后 handler 的 BindAndValidate 仍可读。
//
// 账号必须与业务层用同一套规范化（TrimSpace + 小写）：否则攻击者给登录名加一个
// 前导空格就能拿到新的限流桶（业务查询会 TrimSpace 后照常命中同一账号），
// 10/min/账号的防爆破形同虚设。
func AccountKeyExtractor(c *app.RequestContext) (string, string, bool) {
	var payload struct {
		Identifier string `json:"identifier"`
		Email      string `json:"email"`
		Username   string `json:"username"`
	}
	body, _ := c.Body()
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", false
	}
	for _, account := range []string{payload.Identifier, payload.Email, payload.Username} {
		if normalized := NormalizeAccountKey(account); normalized != "" {
			return "account", normalized, true
		}
	}
	return "", "", false
}

// NormalizeAccountKey 是账号维度的唯一规范化实现：登录名/邮箱大小写不敏感、
// 前后空白忽略。限流 key 与后续业务查询必须使用同一规则。
func NormalizeAccountKey(account string) string {
	return strings.ToLower(strings.TrimSpace(account))
}

// RateLimit 限流中间件。维度选择：
//   - 传入 extractor 且提取成功 → 用提取的维度（如按账号）
//   - ByUser=true 且已登录 → 按 user_id（写接口：每个用户自己的配额）
//   - ByUser=true 但匿名   → 回退按 IP（未登录用户共享 IP 配额）
//   - 其余                → 按 IP
//
// 被限流 → 429 RATE_LIMITED（response 已定义映射）。
// 限流依赖故障（Redis 挂）→ fail-open 放行 + Warn：限流失效比接口不可用好，
// 认证类场景（login）若要 fail-closed（拒绝所有请求防暴力破解）需另行配置。
func RateLimit(l Limiter, rule RateLimitRule, extract ...KeyExtractor) app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		key := rateLimitKey(ctx, rule)
		if len(extract) > 0 && !rule.ByUser {
			if dim, id, ok := extract[0](ctx); ok {
				key = fmt.Sprintf("ratelimit:%s:%s:%s", rule.Scope, dim, id)
			}
		}
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
