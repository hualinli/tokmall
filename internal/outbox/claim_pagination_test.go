package outbox_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/testsupport"
)

// 超时消息排在新鲜 PEL 后面，必须能跨页恢复；投递次数也必须来自该消息，
// 不能只查 PEL 的前 256 项。无需业务 handler，超限消息直接进 DLQ。
func TestClaimOnceScansTailAndWraps(t *testing.T) {
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: testsupport.RedisAddr(t)})
	t.Cleanup(func() { _ = client.Close() })
	stream, group := "claim-pagination:"+uuid.NewString(), "review"
	dlq := stream + ":dlq"
	t.Cleanup(func() { _ = client.Del(ctx, stream, dlq).Err() })
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	ids := make([]string, 321)
	for i := range ids {
		id, err := client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{
			"event_id": uuid.NewString(), "event_type": outbox.EventTypeOrderPaid,
			"aggregate_id": "1", "payload": "{}",
		}}).Result()
		require.NoError(t, err)
		ids[i] = id
	}
	_, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: "alive", Streams: []string{stream, ">"}, Count: 321,
	}).Result()
	require.NoError(t, err)
	consumer := outbox.NewConsumer(nil, client, stream, group, dlq, "recovery", nil)
	for i, target := range []string{ids[320], ids[0]} {
		// 第一次验证尾部，第二次验证游标绕回后仍能处理较早的消息。
		require.NoError(t, client.Do(ctx, "XCLAIM", stream, group, "dead", 0, target,
			"IDLE", 60000, "RETRYCOUNT", 8).Err())
		processed := 0
		for range 12 {
			n, claimErr := consumer.ClaimOnce(ctx)
			require.NoError(t, claimErr)
			processed += n
		}
		require.Equal(t, 1, processed)
		require.Equal(t, int64(i+1), dlqLen(t, client, dlq))
		require.Equal(t, int64(320-i), pendingLen(t, client, stream, group))
	}
	entries, err := client.XRange(ctx, dlq, "-", "+").Result()
	require.NoError(t, err)
	for _, entry := range entries {
		require.Equal(t, "8", entry.Values["dlq_deliveries"])
	}
}
