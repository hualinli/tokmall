package outbox_test

// DLQ 上限的回归测试。
//
// 背景：旧实现曾经把 XPendingExt 的错误吞掉并返回空 map。判定式是
// `deliveries[id] > maxDeliveries`，map 未命中得 0，于是毒丸消息永远达不到阈值——
// DLQ 安全网被**静默**关闭（无日志、无指标），一条稳定失败的消息无限重投。
//
// 这里用 go-redis 的 AddHook 精确让 XPENDING 失败（XCLAIM 保持正常），
// 模拟"Redis 抖动导致投递次数读不到"，不需要改动生产代码。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/lihualin/tokmall/internal/testsupport"
)

// alwaysFailHandler 永远失败：模拟"重投也不会成功"的毒丸消息。
type alwaysFailHandler struct{}

func (alwaysFailHandler) EventTypes() []string { return []string{outbox.EventTypeOrderPaid} }
func (alwaysFailHandler) Handle(_ context.Context, _ *gorm.DB, _ outbox.Event) error {
	return errors.New("poison: this handler always fails")
}

// xpendingFailHook 只让 XPENDING 命令失败。
type xpendingFailHook struct {
	armed bool
	hits  int
}

func (h *xpendingFailHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *xpendingFailHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.armed && cmd.Name() == "xpending" {
			h.hits++
			return errors.New("simulated XPENDING failure (redis hiccup)")
		}
		return next(ctx, cmd)
	}
}

func (h *xpendingFailHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// newPoisonEnv 造一条投递次数已远超 maxDeliveries、但仍在 PEL 里的毒丸消息。
//
// 投递次数用**裸客户端**的 XAUTOCLAIM 顶上去，不能用 consumer.ClaimOnce：
// 后者一旦判定超限就会自己 DLQ + ACK，PEL 被清空，就再也观察不到判定过程。
func newPoisonEnv(t *testing.T, db *gorm.DB, client *redis.Client, label string, deliveries int64) (*outbox.Consumer, string) {
	t.Helper()
	ctx := context.Background()
	stream := "dlq:stream:" + label
	group := "dlq-group:" + label
	dlq := stream + ":dlq"

	consumer := outbox.NewConsumer(db, client, stream, group, dlq, "dlq-consumer",
		[]outbox.Handler{alwaysFailHandler{}},
		outbox.WithConsumerClaimIdle(0), outbox.WithConsumerBlock(10*time.Millisecond))
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	t.Cleanup(func() { _ = client.Del(ctx, stream, dlq).Err() })

	_, err := client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{
		"event_id": "11111111-1111-1111-1111-111111111111", "event_type": outbox.EventTypeOrderPaid,
		"aggregate_type": "order", "aggregate_id": "1", "payload": "{}",
	}}).Result()
	require.NoError(t, err)

	// 消费一次：handler 失败 → 不 ACK → 消息带着 RetryCount=1 留在 PEL。
	_, err = consumer.ConsumeOnce(ctx)
	require.NoError(t, err)

	// 用裸客户端把投递次数顶上去，不触发 DLQ 逻辑。
	for range deliveries - 1 {
		_, _, err := client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: stream, Group: group, Consumer: "bump", MinIdle: 0, Start: "0-0", Count: 1,
		}).Result()
		require.NoError(t, err)
	}
	return consumer, dlq
}

func dlqLen(t *testing.T, client *redis.Client, dlq string) int64 {
	t.Helper()
	n, err := client.XLen(context.Background(), dlq).Result()
	require.NoError(t, err)
	return n
}

// measured 用一个**不带 hook** 的旁路 client 读取状态。
// 观测必须走它：拿故障 client 自己测，会把我们注入的故障也一起读回来。
func measured(t *testing.T, addr string) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Ping(context.Background()).Err())
	return c
}

func pendingLen(t *testing.T, client *redis.Client, stream, group string) int64 {
	t.Helper()
	info, err := client.XPending(context.Background(), stream, group).Result()
	require.NoError(t, err)
	return info.Count
}

func retryCount(t *testing.T, client *redis.Client, stream, group string) int64 {
	t.Helper()
	pending, err := client.XPendingExt(context.Background(), &redis.XPendingExtArgs{
		Stream: stream, Group: group, Start: "-", End: "+", Count: 10,
	}).Result()
	require.NoError(t, err)
	require.NotEmpty(t, pending)
	return pending[0].RetryCount
}

// TestClaimOnce_UnreadableDeliveriesMustNotBeTreatedAsZero 是核心一格：
// 投递次数读不到时，不能把未知当成 0（那会让毒丸消息无限重投），
// 也不能当成超限（那会误杀健康消息）——正确做法是本轮不动消息。
func TestClaimOnce_UnreadableDeliveriesMustNotBeTreatedAsZero(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	addr := testsupport.RedisAddr(t)

	hook := &xpendingFailHook{}
	client := redis.NewClient(&redis.Options{Addr: addr})
	client.AddHook(hook)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	const stream, group = "dlq:stream:faulty", "dlq-group:faulty"
	consumer, dlq := newPoisonEnv(t, db, client, "faulty", 8)
	observer := measured(t, addr)

	before := retryCount(t, observer, stream, group)
	require.Greater(t, before, int64(5), "前置条件：投递次数必须已超过 maxDeliveries(5)")

	hook.armed = true
	processed, err := consumer.ClaimOnce(context.Background())

	// 断言 1：必须把错误往上抛，而不是吞掉后继续按 0 判定。
	require.Error(t, err, "投递次数读不到时 ClaimOnce 必须返回错误，不能静默继续")
	require.ErrorContains(t, err, "read pending deliveries")
	require.Positive(t, hook.hits, "本用例必须真的注入到 XPENDING 失败")

	// 断言 2：既不重投（那等于按 0 判定），也不误杀。
	require.Zero(t, processed, "读不到投递次数就不该处理任何消息")
	require.Zero(t, dlqLen(t, observer, dlq), "读不到投递次数时不得把消息判为死信")

	// 断言 3：消息仍留在 PEL，且投递次数没有被无谓地推高
	// （正确的实现要先读次数再认领，避免认领本身让计数失真）。
	require.Equal(t, int64(1), pendingLen(t, observer, stream, group), "消息应仍留在 PEL 里等下一轮")
	require.Equal(t, before, retryCount(t, observer, stream, group), "读不到次数时不应认领并推高计数")

	// 断言 4：Redis 恢复后，死信逻辑照常生效，不需要人工干预。
	hook.armed = false
	processed, err = consumer.ClaimOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, int64(1), dlqLen(t, observer, dlq),
		"投递次数查询恢复后，同一条消息应立即被正确判定为死信")
}

// TestClaimOnce_HealthyRetryStillWorks 确认这次改动没有妨碍正常路径：
// 未超投递上限的消息照常被认领并重投。
func TestClaimOnce_HealthyRetryStillWorks(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	addr := testsupport.RedisAddr(t)

	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	const stream, group = "dlq:stream:healthy", "dlq-group:healthy"
	consumer, dlq := newPoisonEnv(t, db, client, "healthy", 3)
	observer := measured(t, addr)

	before := retryCount(t, observer, stream, group)
	require.Less(t, before, int64(5), "前置条件：投递次数必须未超过上限")

	processed, err := consumer.ClaimOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, processed, "未超上限的消息应被重投")
	require.Zero(t, dlqLen(t, observer, dlq), "未超上限不得进死信")
	require.Equal(t, int64(1), pendingLen(t, observer, stream, group), "重投后仍留在 PEL")
}
