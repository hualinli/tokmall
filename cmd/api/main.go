package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	hertzslog "github.com/hertz-contrib/logger/slog"
	hertzmonitor "github.com/hertz-contrib/monitor-prometheus"
	"github.com/hertz-contrib/requestid"
	"github.com/lihualin/tokmall/internal/api"
	"github.com/lihualin/tokmall/internal/attribution"
	"github.com/lihualin/tokmall/internal/cache"
	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/database"
	"github.com/lihualin/tokmall/internal/feed"
	"github.com/lihualin/tokmall/internal/health"
	"github.com/lihualin/tokmall/internal/interaction"
	"github.com/lihualin/tokmall/internal/inventory"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/lihualin/tokmall/internal/observability"
	"github.com/lihualin/tokmall/internal/order"
	"github.com/lihualin/tokmall/internal/product"
	"github.com/lihualin/tokmall/internal/promotion"
	"github.com/lihualin/tokmall/internal/storage"
	"github.com/lihualin/tokmall/internal/token"
	"github.com/lihualin/tokmall/internal/user"
	"github.com/lihualin/tokmall/internal/video"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"gopkg.in/natefinch/lumberjack.v2"
	"gorm.io/gorm"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ranker, err := newRanker(cfg.FeedRankStrategy)
	if err != nil {
		return fmt.Errorf("configure feed ranker: %w", err)
	}
	logger := &lumberjack.Logger{
		Filename:   "logs/app.log",
		MaxSize:    20,
		MaxBackups: 5,
		MaxAge:     10,
		Compress:   true,
	}
	textHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	jsonHandler := slog.NewJSONHandler(logger, &slog.HandlerOptions{Level: slog.LevelDebug})
	multiHandler := slog.NewMultiHandler(textHandler, jsonHandler)
	slog.SetDefault(slog.New(multiHandler))
	hlog.SetLogger(hertzslog.NewLogger(
		hertzslog.WithOutput(logger),
		hertzslog.WithHandlerOptions(&slog.HandlerOptions{Level: slog.LevelDebug}),
	))

	metricsRegistry := prometheus.NewRegistry()
	db, err := database.Connect(cfg.DBConfig)
	if err != nil {
		return fmt.Errorf("connect databse: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get sql db: %w", err)
	}
	metricsRegistry.MustRegister(observability.NewDBPoolCollector(sqlDB, "tokmall"))
	err = database.Migrate(db)
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database migrate: %w", err)
	}
	redisClient, err := cache.NewClient(cfg.RedisConfig)
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer redisClient.Close()
	addr := fmt.Sprintf(":%s", cfg.ServerPort)
	h := server.New(
		server.WithHostPorts(addr),
		server.WithTracer(hertzmonitor.NewServerTracer(
			":9091", "/metrics",
			hertzmonitor.WithRegistry(metricsRegistry),
			hertzmonitor.WithEnableGoCollector(true),
		)),
	)
	slog.Info("server starting", "port", cfg.ServerPort)
	h.Use(
		middleware.Recovery(),
		requestid.New(
			requestid.WithGenerator(func(ctx context.Context, c *app.RequestContext) string {
				return uuid.Must(uuid.NewV7()).String()
			}),
		),
	)
	storageClient, err := storage.New(cfg.MinIOConfig)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}
	// 注意：必须用 nil 接口变量。直接声明 *feed.RedisFeedCache 再赋给接口，
	// 关闭时会得到"typed nil"——接口判 nil 为真但方法调用即 panic。
	var feedCache feed.FirstPageCache
	var feedInvalidator video.FeedCacheInvalidator
	if cfg.FeedCacheEnabled {
		redisFeedCache := feed.NewRedisFeedCache(redisClient, cfg.FeedCacheTTL)
		feedCache = redisFeedCache
		feedInvalidator = redisFeedCache
	}
	followingCache, followingFeedInvalidator := assembleFollowingFeedCache(cfg.RedisConfig, redisClient)
	// 限流器（M7）：Redis 固定窗口计数；关闭（压测/调试）时 nil，registerRoutes 不挂载。
	var rateLimiter middleware.Limiter
	if cfg.RateLimitEnabled {
		rateLimiter = middleware.NewRedisLimiter(redisClient)
	}
	tokenManager := token.NewManager([]byte(cfg.JWTSecret))
	videoService := video.NewService(
		video.NewRepository(db),
		storageClient,
		cfg.UploadURLExpiry,
		cfg.CursorSecret,
		feedInvalidator,
	)
	videoService.SetProductCardReader(video.NewProductCardReader(db))
	userHandler := user.NewHandler(user.NewService(
		user.NewRepository(db),
		tokenManager,
		user.NewRedisRefreshTokenBlacklist(redisClient),
		storageClient,
		videoService,
	))
	feedService := feed.NewService(
		feed.NewRepository(db),
		feed.NewCodec(cfg.CursorSecret),
		storageClient,
		feedCache,
		followingCache,
		observability.NewFeedCacheStats(metricsRegistry),
		ranker,
	)
	feedService.SetProductCardReader(feed.NewProductCardReader(db))
	feedHandler := feed.NewHandler(feedService)
	videoHandler := video.NewHandler(videoService)
	interactionHandler := interaction.NewHandler(interaction.NewService(
		interaction.NewRepository(db),
		storageClient,
		cfg.CursorSecret,
		followingFeedInvalidator,
	))
	productHandler := product.NewHandler(product.NewService(
		product.NewRepository(db),
		storageClient,
		videoService, // ImageTicketValidator：封面防伪造复用视频模块的凭证核销
	))
	inventoryHandler := inventory.NewHandler(inventory.NewService(
		inventory.NewRepository(db),
	))
	orderRepository := order.NewRepository(db)
	orderHandler := order.NewHandler(order.NewService(orderRepository))
	promotionHandler := promotion.NewHandler(promotion.NewService(promotion.NewRepository(db)))
	attributionHandler := attribution.NewHandler(attribution.NewService(attribution.NewRepository(db)))
	var expiryWorkerCancel context.CancelFunc
	var expiryWorkerDone chan struct{}
	if cfg.OrderExpiryWorkerEnabled {
		expiryWorker := order.NewExpiryWorker(orderRepository, cfg.OrderExpiryWorkerInterval, cfg.OrderExpiryWorkerBatchSize)
		workerCtx, cancel := context.WithCancel(ctx)
		expiryWorkerCancel = cancel
		expiryWorkerDone = make(chan struct{})
		go func() {
			defer close(expiryWorkerDone)
			expiryWorker.Run(workerCtx)
		}()
	}
	registerRoutes(h, routes{
		db:          db,
		token:       tokenManager,
		user:        userHandler,
		video:       videoHandler,
		feed:        feedHandler,
		interaction: interactionHandler,
		product:     productHandler,
		inventory:   inventoryHandler,
		order:       orderHandler,
		promotion:   promotionHandler,
		attribution: attributionHandler,
		rateLimiter: rateLimiter,
		rateRules: rateRules{
			authAccount: middleware.RateLimitRule{Scope: "auth-account", Limit: cfg.AuthAccountLimit, Window: time.Minute},
			authIP:      middleware.RateLimitRule{Scope: "auth-ip", Limit: cfg.AuthIPLimit, Window: time.Minute},
			read:        middleware.RateLimitRule{Scope: "read", Limit: cfg.ReadRateLimit, Window: time.Minute},
			write:       middleware.RateLimitRule{Scope: "write", Limit: cfg.WriteRateLimit, Window: 10 * time.Second, ByUser: true},
		},
	})
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.Run()
	}()
	select {
	case err := <-errCh:
		if stopErr := stopExpiryWorker(expiryWorkerCancel, expiryWorkerDone, 30*time.Second); stopErr != nil {
			slog.Error("expiry worker did not stop after server exit", "err", stopErr)
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		shutdownErr := h.Shutdown(shutdownCtx)
		workerStopErr := stopExpiryWorker(expiryWorkerCancel, expiryWorkerDone, 30*time.Second)
		if shutdownErr != nil {
			return fmt.Errorf("shutdown: %w", shutdownErr)
		}
		if workerStopErr != nil {
			return workerStopErr
		}
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
		slog.Info("server stopped gracefully")
		return nil
	}
}

func stopExpiryWorker(cancel context.CancelFunc, done <-chan struct{}, timeout time.Duration) error {
	if cancel == nil {
		return nil
	}
	cancel()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errors.New("order expiry worker shutdown timed out")
	}
}

// assembleFollowingFeedCache 在 main 统一装配 feed 的缓存实现，并把同一个实例
// 以 interaction 消费侧接口注入关注服务。缓存关闭时返回真正的 nil 接口，避免
// typed nil 在关注写路径上触发方法调用；关闭缓存只损失性能，不改变关注正确性。
func assembleFollowingFeedCache(cfg config.RedisConfig, redisClient *redis.Client) (feed.FollowingFeedCache, interaction.FollowingFeedInvalidator) {
	if !cfg.FollowingCacheEnabled || redisClient == nil {
		return nil, nil
	}
	c := feed.NewRedisFollowingFeedCache(redisClient, cfg.FeedCacheTTL)
	return c, c
}

// newRanker 根据生产配置装配排序器。
// 当前只有 time 能与关注 Feed 的 (published_at,id) 游标组成正确的在线分页。
// HotRanker 保留在 feed 包中做排序实验，但在分页语义闭合前拒绝启动。
func newRanker(strategy string) (feed.Ranker, error) {
	switch strategy {
	case "time":
		return feed.TimeRanker{}, nil
	case "hot":
		return nil, fmt.Errorf("hot rank strategy is experimental and does not support cursor pagination")
	default:
		return nil, fmt.Errorf("unknown feed rank strategy %q", strategy)
	}
}

// routes 集中持有各模块 handler，registerRoutes 是唯一的路由注册点：
// 同步测试（main_test.go）用 nil handler 复用同一注册逻辑，
// 保证 openapi.yaml 与代码路由不脱节。rateLimiter 为 nil 时（限流关闭）
// 不挂任何限流中间件，route-sync 测试不受影响。
type routes struct {
	db          *gorm.DB
	token       *token.Manager
	user        *user.Handler
	video       *video.Handler
	feed        *feed.Handler
	interaction *interaction.Handler
	product     *product.Handler
	inventory   *inventory.Handler
	order       *order.Handler
	promotion   *promotion.Handler
	attribution *attribution.Handler
	rateLimiter middleware.Limiter
	rateRules   rateRules
}

// rateRules 限流规则（挂载点见 registerRoutes）：
//   - authAccount：按账号 10/min（防暴力破解——爆破的维度是账号不是 IP，NAT 下多人
//     共享 IP 按 IP 限会误伤真实用户）；
//   - authIP：按 IP 100/min（放宽版，防单一来源洪峰）；
//   - read：按 IP 6000/min（只挡爬虫/洪峰，阈值远高于真实用户行为）；
//   - write：按用户 100/10s（登录写操作，天然低频）。
type rateRules struct {
	authAccount middleware.RateLimitRule
	authIP      middleware.RateLimitRule
	read        middleware.RateLimitRule
	write       middleware.RateLimitRule
}

// authMW 构造 auth 子组的限流中间件（未启用限流时为空，行为与现状一致）。
// 双规则叠加：按账号（精确打击）+ 按 IP（洪峰防护），两道独立计数。
func authMW(r routes) []app.HandlerFunc {
	if r.rateLimiter == nil {
		return nil
	}
	return []app.HandlerFunc{
		middleware.RateLimit(r.rateLimiter, r.rateRules.authAccount, middleware.AccountKeyExtractor),
		middleware.RateLimit(r.rateLimiter, r.rateRules.authIP),
	}
}

func registerRoutes(h *server.Hertz, r routes) {
	health.NewHandler(r.db).Register(h)
	g := h.Group("/api/v1")
	g.GET("/docs/openapi.yaml", func(ctx context.Context, c *app.RequestContext) {
		c.Response.Header.SetContentType("application/yaml")
		c.Response.SetBody(api.Spec)
	})
	// 读接口限流（IP 级）：挂在 g 上，auth 子组与 optional/protected 组自动继承；
	// auth 子组再叠加更严的 auth 规则（10/min 先于 read 200/10s 触发，两道都独立计数）。
	if r.rateLimiter != nil {
		g.Use(middleware.RateLimit(r.rateLimiter, r.rateRules.read))
	}
	r.user.RegisterRoutes(g, authMW(r)...)
	optional := g.Group("/", middleware.AuthOptional(r.token))
	r.user.RegisterOptionalRoutes(optional)
	r.video.RegisterOptionalRoutes(optional)
	r.interaction.RegisterOptionalRoutes(optional)
	r.product.RegisterOptionalRoutes(optional)
	r.promotion.RegisterOptionalRoutes(optional)
	protected := g.Group("/", middleware.AuthMiddleware(r.token))
	if r.rateLimiter != nil {
		// 写接口：用户级限流（ByUser，登录后按 user_id）
		protected.Use(middleware.RateLimit(r.rateLimiter, r.rateRules.write))
	}
	r.user.RegisterProtectedRoutes(protected)
	r.video.RegisterProtectedRoutes(protected)
	r.feed.RegisterRoutes(g)
	r.feed.RegisterProtectedRoutes(protected)
	r.interaction.RegisterProtectedRoutes(protected)
	r.product.RegisterProtectedRoutes(protected)
	r.inventory.RegisterProtectedRoutes(protected)
	r.order.RegisterProtectedRoutes(protected)
	r.promotion.RegisterProtectedRoutes(protected)
	r.attribution.RegisterProtectedRoutes(protected)
}
