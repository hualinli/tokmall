package user

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RefreshTokenBlacklist 消费侧接口：service 只依赖它，不感知 Redis。
type RefreshTokenBlacklist interface {
	Block(ctx context.Context, jti string, ttl time.Duration) error
	IsBlocked(ctx context.Context, jti string) (bool, error)
}

// RedisRefreshTokenBlacklist 黑名单的 Redis 实现。
// key: blacklist:refresh:{jti}，TTL = token 剩余寿命：到期自动消失，
// 不需要任何清理任务，这是"TTL 即生命周期"的典型用法。
type RedisRefreshTokenBlacklist struct {
	client *redis.Client
}

func NewRedisRefreshTokenBlacklist(client *redis.Client) *RedisRefreshTokenBlacklist {
	return &RedisRefreshTokenBlacklist{client: client}
}

func (b *RedisRefreshTokenBlacklist) Block(ctx context.Context, jti string, ttl time.Duration) error {
	// SET key val EX ttl NX 一条命令原子完成：
	// NX 保证已存在的黑名单（更长的 TTL）不会被重复 Block 缩短。
	err := b.client.SetNX(ctx, refreshBlacklistKey(jti), "1", ttl).Err()
	if err != nil {
		return fmt.Errorf("set refresh blacklist: %w", err)
	}
	return nil
}

func (b *RedisRefreshTokenBlacklist) IsBlocked(ctx context.Context, jti string) (bool, error) {
	n, err := b.client.Exists(ctx, refreshBlacklistKey(jti)).Result()
	if err != nil {
		return false, fmt.Errorf("exists refresh blacklist: %w", err)
	}
	return n > 0, nil
}

func refreshBlacklistKey(jti string) string {
	return "blacklist:refresh:" + jti
}
