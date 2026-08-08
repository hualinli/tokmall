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
	"github.com/lihualin/tokmall/internal/cache"
	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/database"
	"github.com/lihualin/tokmall/internal/feed"
	"github.com/lihualin/tokmall/internal/health"
	"github.com/lihualin/tokmall/internal/interaction"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/lihualin/tokmall/internal/observability"
	"github.com/lihualin/tokmall/internal/storage"
	"github.com/lihualin/tokmall/internal/token"
	"github.com/lihualin/tokmall/internal/user"
	"github.com/lihualin/tokmall/internal/video"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/natefinch/lumberjack.v2"
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
	g := h.Group("/api/v1")
	health.NewHandler(db).Register(h)
	tokenManager := token.NewManager([]byte(cfg.JWTSecret))
	userHandler := user.NewHandler(user.NewService(
		user.NewRepository(db),
		tokenManager,
		user.NewRedisRefreshTokenBlacklist(redisClient),
	))
	userHandler.RegisterRoutes(g)
	protected := g.Group("/", middleware.AuthMiddleware(tokenManager))
	userHandler.RegisterProtectedRoutes(protected)

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
	feedHandler := feed.NewHandler(feed.NewService(
		feed.NewRepository(db),
		feed.NewCodec(cfg.CursorSecret),
		storageClient,
		feedCache,
		observability.NewFeedCacheStats(metricsRegistry),
	))
	feedHandler.RegisterRoutes(g)

	videoHandler := video.NewHandler(video.NewService(
		video.NewRepository(db),
		storageClient,
		cfg.UploadURLExpiry,
		cfg.CursorSecret,
		feedInvalidator,
	))
	videoHandler.RegisterProtectedRoutes(protected)

	interactionHandler := interaction.NewHandler(interaction.NewService(interaction.NewRepository(db)))
	interactionHandler.RegisterProtectedRoutes(protected)
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.Run()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
		slog.Info("server stopped gracefully")
		return nil
	}
}
