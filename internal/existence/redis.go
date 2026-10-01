package existence

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisMissingMarker 是负缓存的 Redis 实现。
//
// key 形如 existence:missing:{namespace}:{id}，值固定为 "1"，靠 TTL 自动消失——
// 这样不需要任何清理任务，也不会像"永久负缓存"那样在实体后来被创建时
// 一直返回 404（TTL 就是可容忍的错误窗口）。
type RedisMissingMarker struct {
	client redis.UniversalClient
}

func NewRedisMissingMarker(client redis.UniversalClient) *RedisMissingMarker {
	return &RedisMissingMarker{client: client}
}

func missingKey(namespace string, id int64) string {
	return "existence:missing:" + namespace + ":" + strconv.FormatInt(id, 10)
}

func (m *RedisMissingMarker) MarkMissing(ctx context.Context, namespace string, id int64, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	if err := m.client.Set(ctx, missingKey(namespace, id), "1", ttl).Err(); err != nil {
		return fmt.Errorf("mark missing: %w", err)
	}
	return nil
}

func (m *RedisMissingMarker) IsMissing(ctx context.Context, namespace string, id int64) (bool, error) {
	n, err := m.client.Exists(ctx, missingKey(namespace, id)).Result()
	if err != nil {
		return false, fmt.Errorf("check missing: %w", err)
	}
	return n > 0, nil
}
