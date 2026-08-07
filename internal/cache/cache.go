package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/lihualin/tokmall/internal/config"
	"github.com/redis/go-redis/v9"
)

// NewClient 创建 Redis 客户端并做连通性检查。
// 模块不直接持有原始 client 的玩法在这里终结：模块各自定义消费侧接口，
// main.go 装配时把 client 注入对应的 Redis 实现。
func NewClient(cfg config.RedisConfig) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
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
