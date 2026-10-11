package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/lihualin/tokmall/internal/api"
	"github.com/lihualin/tokmall/internal/attribution"
	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/feed"
	"github.com/lihualin/tokmall/internal/interaction"
	"github.com/lihualin/tokmall/internal/inventory"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/lihualin/tokmall/internal/order"
	"github.com/lihualin/tokmall/internal/product"
	"github.com/lihualin/tokmall/internal/promotion"
	"github.com/lihualin/tokmall/internal/user"
	"github.com/lihualin/tokmall/internal/video"
)

func TestStopWorkerIsBounded(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		require.NoError(t, stopWorker("test", nil, nil, time.Millisecond))
	})
	t.Run("stops", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			<-ctx.Done()
			close(done)
		}()
		require.NoError(t, stopWorker("test", cancel, done, time.Second))
	})
	t.Run("timeout", func(t *testing.T) {
		err := stopWorker("outbox relay", func() {}, make(chan struct{}), time.Millisecond)
		require.Error(t, err)
		require.ErrorContains(t, err, "outbox relay")
	})
}

// TestBackgroundWorkersStopInReverseOrder：停机顺序是"消费者 → 中继 → 关单"，
// 先停止读模型写入再停止投递，避免停机瞬间产生没人消费的事件。
func TestBackgroundWorkersStopInReverseOrder(t *testing.T) {
	var stopped []string
	workers := &backgroundWorkers{}
	for _, name := range []string{"expiry", "relay", "consumer"} {
		n := name
		workers.stops = append(workers.stops, func(time.Duration) error {
			stopped = append(stopped, n)
			return nil
		})
	}
	require.NoError(t, workers.stopAll(time.Second))
	require.Equal(t, []string{"consumer", "relay", "expiry"}, stopped)
}

// TestBackgroundWorkersReportFirstError：任一 worker 停机超时都要报出来，
// 但不能因为第一个错误就跳过其余的停机尝试。
func TestBackgroundWorkersReportFirstError(t *testing.T) {
	var stopped int
	workers := &backgroundWorkers{}
	for i := range 3 {
		workers.stops = append(workers.stops, func(time.Duration) error {
			stopped++
			if i == 0 {
				return errors.New("first worker timed out")
			}
			return nil
		})
	}
	err := workers.stopAll(time.Second)
	require.ErrorContains(t, err, "first worker timed out")
	require.Equal(t, 3, stopped, "其余 worker 仍必须被停止")
}

type openAPIDoc struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

func TestNewRanker_TimeIsProductionDefault(t *testing.T) {
	ranker, err := newRanker("time")
	require.NoError(t, err)
	require.IsType(t, feed.TimeRanker{}, ranker)
	require.True(t, ranker.SupportsCursorPagination())
}

func TestNewRanker_RejectsHotExperiment(t *testing.T) {
	ranker, err := newRanker("hot")
	require.Error(t, err)
	require.Nil(t, ranker)
	require.ErrorContains(t, err, "does not support cursor pagination")
}

func TestNewRanker_RejectsUnknownStrategy(t *testing.T) {
	ranker, err := newRanker("unknown")
	require.Error(t, err)
	require.Nil(t, ranker)
	require.ErrorContains(t, err, "unknown feed rank strategy")
}

var hertzParamRe = regexp.MustCompile(`:([^/]+)`)

// TestOpenAPISync 契约同步测试：用与 run() 完全相同的注册逻辑（nil handler）
// 生成运行时路由表，与 openapi.yaml 的 paths 双向 diff——
// 路由表有 yaml 没有 = 忘写文档；yaml 有路由表没有 = 文档残留。
func TestOpenAPISync(t *testing.T) {
	var doc openAPIDoc
	require.NoError(t, yaml.Unmarshal(api.Spec, &doc), "openapi.yaml 必须是合法 yaml")

	h := server.New()
	registerRoutes(h, routes{
		user:        user.NewHandler(nil),
		video:       video.NewHandler(nil),
		feed:        feed.NewHandler(nil),
		interaction: interaction.NewHandler(nil),
		product:     product.NewHandler(nil),
		inventory:   inventory.NewHandler(nil),
		order:       order.NewHandler(nil),
		promotion:   promotion.NewHandler(nil),
		attribution: attribution.NewHandler(nil),
	})

	routeKeys := map[string]bool{}
	for _, r := range h.Routes() {
		method := strings.ToUpper(r.Method)
		if method == "HEAD" || method == "OPTIONS" {
			continue
		}
		routeKeys[method+" "+hertzParamRe.ReplaceAllString(r.Path, "{$1}")] = true
	}

	docKeys := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			docKeys[strings.ToUpper(method)+" "+path] = true
		}
	}

	var missing, stale []string
	for k := range routeKeys {
		if !docKeys[k] {
			missing = append(missing, k)
		}
	}
	for k := range docKeys {
		if !routeKeys[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	require.Empty(t, missing, "路由表存在但 openapi.yaml 缺失（改端点必须同步改文档）")
	require.Empty(t, stale, "openapi.yaml 存在但路由表没有（文档残留，端点已删除）")
}

func TestAssembleFollowingFeedCacheInjectsSameImplementation(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	cfg := config.RedisConfig{FollowingCacheEnabled: true, FeedCacheTTL: time.Minute}

	cache, invalidator := assembleFollowingFeedCache(cfg, client)
	require.NotNil(t, cache)
	require.NotNil(t, invalidator)
	require.Same(t, cache, invalidator, "feed cache 与 interaction invalidator 必须共享同一实现")

	disabled := cfg
	disabled.FollowingCacheEnabled = false
	cache, invalidator = assembleFollowingFeedCache(disabled, client)
	require.Nil(t, cache)
	require.Nil(t, invalidator)

	cache, invalidator = assembleFollowingFeedCache(cfg, nil)
	require.Nil(t, cache)
	require.Nil(t, invalidator)
}

// 使用实际装配，锁住 IP 超限时不再产生账号 key 的顺序。
type authRejectingLimiter struct{ keys []string }

func (l *authRejectingLimiter) Allow(_ context.Context, key string, _ int, _ time.Duration) (bool, error) {
	l.keys = append(l.keys, key)
	return false, nil
}
func TestAuthMiddlewareIPRejectsBeforeAccountKey(t *testing.T) {
	limiter := &authRejectingLimiter{}
	h := server.New()
	rt := routes{rateLimiter: limiter, rateRules: rateRules{
		authIP:      middleware.RateLimitRule{Scope: "auth-ip", Limit: 100, Window: time.Minute},
		authAccount: middleware.RateLimitRule{Scope: "auth-account", Limit: 10, Window: time.Minute},
	}}
	// 与登录路由同构：IP 配额在前，账号配额在后。
	mw := append(authIPMW(rt), authCredentialMW(rt)...)
	h.POST("/login", append(mw, func(_ context.Context, c *app.RequestContext) { c.Status(http.StatusOK) })...)
	body := `{"identifier":"alice"}`
	w := ut.PerformRequest(h.Engine, http.MethodPost, "/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)})
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Len(t, limiter.keys, 1)
	require.True(t, strings.HasPrefix(limiter.keys[0], "ratelimit:auth-ip:ip:"))
}
func TestAuthMiddlewareBodyLimitWithoutRateLimiter(t *testing.T) {
	h := server.New()
	h.POST("/login", append(authIPMW(routes{}), func(_ context.Context, c *app.RequestContext) { c.Status(http.StatusOK) })...)
	body := strings.Repeat("a", (16<<10)+1)
	w := ut.PerformRequest(h.Engine, http.MethodPost, "/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)})
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestAuthMiddlewareRedisKeyBounds(t *testing.T) {
	if os.Getenv("TOKMALL_RUN_REDIS_INTEGRATION") != "1" {
		t.Skip("Redis integration is disabled")
	}
	addr := os.Getenv("TOKMALL_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())
	scope := "test-auth-" + uuid.NewString()
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		// 只删除本测试命名空间的 key。
		keys, err := client.Keys(cleanupCtx, "ratelimit:"+scope+"*:*").Result()
		if err == nil && len(keys) > 0 {
			require.NoError(t, client.Del(cleanupCtx, keys...).Err())
		}
	})
	h := server.New()
	rules := rateRules{
		authIP:      middleware.RateLimitRule{Scope: scope + "-ip", Limit: 1, Window: time.Minute},
		authAccount: middleware.RateLimitRule{Scope: scope + "-account", Limit: 10, Window: time.Minute},
	}
	rt := routes{rateLimiter: middleware.NewRedisLimiter(client), rateRules: rules}
	h.POST("/login", append(append(authIPMW(rt), authCredentialMW(rt)...), func(_ context.Context, c *app.RequestContext) { c.Status(http.StatusOK) })...)
	for i, size := range []int{1 << 20, 8000, 8001} {
		body := `{"identifier":"` + strings.Repeat("a", size) + `"}`
		w := ut.PerformRequest(h.Engine, http.MethodPost, "/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)})
		switch i {
		case 0:
			require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			keys, err := client.Keys(ctx, "ratelimit:"+scope+"*:*").Result()
			require.NoError(t, err)
			require.Empty(t, keys)
		case 1:
			require.Equal(t, http.StatusOK, w.Code)
		case 2:
			require.Equal(t, http.StatusTooManyRequests, w.Code)
		}
	}
	keys, err := client.Keys(ctx, "ratelimit:"+scope+"-account:*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1, "IP 超限请求不得制造第二个账号桶")
	require.Len(t, keys[0], len("ratelimit:"+scope+"-account:account:")+64)
	ttl, err := client.TTL(ctx, keys[0]).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, time.Minute)
}
