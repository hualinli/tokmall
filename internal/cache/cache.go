package cache

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lihualin/tokmall/internal/config"
)

// Option 配置客户端行为。用选项而不是参数：将来加"命令重试策略""连接池大小"
// 这类维度时不需要改所有调用点。
type Option func(*clientOptions)

type clientOptions struct {
	slowThreshold    time.Duration
	slowCommandStats SlowCommandStats
}

// WithSlowCommandLogging 打开慢命令日志（生产路径由 main 装配）。
func WithSlowCommandLogging(threshold time.Duration, stats SlowCommandStats) Option {
	return func(o *clientOptions) {
		o.slowThreshold = threshold
		o.slowCommandStats = stats
	}
}

// NewClient 创建 Redis 客户端并做连通性检查。
// 模块不直接持有原始 client 的玩法在这里终结：模块各自定义消费侧接口，
// main.go 装配时把 client 注入对应的 Redis 实现。
func NewClient(cfg config.RedisConfig, opts ...Option) (*redis.Client, error) {
	options := &redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}
	if cfg.UseTLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	clientOptions := clientOptions{}
	for _, opt := range opts {
		opt(&clientOptions)
	}

	client := redis.NewClient(options)
	if clientOptions.slowCommandStats != nil {
		// Hook 在 client 上注册（go-redis v9 的 Options 没有 AddHook）。
		client.AddHook(NewSlowCommandHook(clientOptions.slowThreshold, clientOptions.slowCommandStats))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		// 失败路径必须清理资源：本进程要退出，但 NewClient 未来可能
		// 被非致命场景（健康检查、worker）使用，不能依赖进程退出兜底。
		_ = client.Close()
		return nil, fmt.Errorf("ping redis %s: %w", cfg.Addr, err)
	}
	return client, nil
}
