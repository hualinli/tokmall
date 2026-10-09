package feed

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/testsupport"
)

// 热榜的 Redis 集成测试：分钟桶、窗口物化、游标翻页稳定性。
//
// 这些性质只有真实 Redis 能验证（ZUNIONSTORE / ZREVRANGEBYSCORE 的语义、
// ZREM 的边界、TTL 行为），内存 fake 证明不了。
//
//	TOKMALL_RUN_REDIS_INTEGRATION=1 go test ./internal/feed -run TestHot -v
func newHotTestClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := testsupport.RedisAddr(t)
	client := redis.NewClient(&redis.Options{Addr: addr, DB: 9})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	require.NoError(t, client.FlushDB(ctx).Err())
	t.Cleanup(func() { _ = client.FlushDB(context.Background()).Err() })
	return client
}

func TestHotKeysAndWeights(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 34, 56, 0, time.UTC)
	require.Equal(t, "feed:hot:v2:minute:202610011234", hotMinuteKey(at))
	require.Equal(t, "feed:hot:v2:window:1790858040", hotWindowKey(at))

	keys := hotMinuteKeys(at)
	require.Len(t, keys, hotWindowMinutes)
	require.Equal(t, "feed:hot:v2:minute:202610011234", keys[len(keys)-1], "最后一个桶是 windowEnd 所在分钟")
	require.Equal(t, "feed:hot:v2:minute:202610011135", keys[0], "第一个桶是 59 分钟前")

	// 权重：点赞 +3 / 取消 -3 / 播放 +1，未知动作不计分。
	require.Equal(t, 3, HotScoreForAction(HotActionLike))
	require.Equal(t, -3, HotScoreForAction(HotActionUnlike))
	require.Equal(t, 1, HotScoreForAction(HotActionPlay))
	require.Zero(t, HotScoreForAction("unknown"))
}

// TestHotWindowMergesMinuteBuckets：窗口 = 最近 60 个分钟桶按成员求和。
func TestHotWindowMergesMinuteBuckets(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

	// 视频 1：5 分钟前 +3、3 分钟前 +3 → 6
	require.NoError(t, hot.AddHotScore(ctx, 1, HotActionLike, now.Add(-5*time.Minute)))
	require.NoError(t, hot.AddHotScore(ctx, 1, HotActionLike, now.Add(-3*time.Minute)))
	// 视频 2：2 分钟前 +1（播放）
	require.NoError(t, hot.AddHotScore(ctx, 2, HotActionPlay, now.Add(-2*time.Minute)))
	// 视频 3：点赞又取消 → 净 0，不应出现在窗口里
	require.NoError(t, hot.AddHotScore(ctx, 3, HotActionLike, now.Add(-4*time.Minute)))
	require.NoError(t, hot.AddHotScore(ctx, 3, HotActionUnlike, now.Add(-4*time.Minute)))

	size, err := hot.MaterializeWindow(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 2, size, "净分 ≤ 0 的条目必须被移除")

	entries, err := hot.Page(ctx, now, nil, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, int64(1), entries[0].VideoID)
	require.Equal(t, int64(6), entries[0].HotScore)
	require.Equal(t, int64(2), entries[1].VideoID)
	require.Equal(t, int64(1), entries[1].HotScore)
}

// TestHotWindowIgnoresExpiredBuckets：窗口只合并最近 60 分钟，更早的桶不参与。
func TestHotWindowIgnoresExpiredBuckets(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

	require.NoError(t, hot.AddHotScore(ctx, 1, HotActionLike, now.Add(-90*time.Minute))) // 窗口外
	require.NoError(t, hot.AddHotScore(ctx, 2, HotActionLike, now.Add(-10*time.Minute))) // 窗口内

	entries, err := hot.Page(ctx, now, nil, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(2), entries[0].VideoID, "窗口外的热度不应计入")
}

func TestHotWindowIsImmutableAndExpiredCursorIsRejected(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)
	ctx := context.Background()
	now := hot.CurrentWindowEnd()
	for id := int64(1); id <= 10; id++ {
		require.NoError(t, hot.AddHotScore(ctx, id, HotActionLike, now))
	}
	first, err := hot.Page(ctx, now, nil, 4)
	require.NoError(t, err)
	cursor := &HotCursor{WindowEnd: now, HotScore: first[3].HotScore, VideoID: first[3].VideoID}
	require.NoError(t, hot.AddHotScore(ctx, 10, HotActionUnlike, now))
	_, err = hot.MaterializeWindow(ctx, now)
	require.NoError(t, err)
	second, err := hot.Page(ctx, now, cursor, 4)
	require.NoError(t, err)
	require.Len(t, second, 4)
	require.Equal(t, int64(6), second[0].VideoID)
	score, err := client.ZScore(ctx, hotWindowKey(now), hotRankMember(10)).Result()
	require.NoError(t, err)
	require.Equal(t, float64(3), score, "materialization must not overwrite an existing snapshot")
	require.NoError(t, client.Del(ctx, hotWindowKey(now)).Err())
	_, err = hot.Page(ctx, now, cursor, 4)
	require.ErrorIs(t, err, ErrInvalidCursor, "expired snapshot must not be reconstructed for an old cursor")
}

// TestHotPageBreaksTiesByVideoID：分数并列时按 video_id 降序，
// 游标据此可以精确跳过已翻过的同分条目。
func TestHotPageBreaksTiesByVideoID(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

	// 5 个视频同分（各 +3）
	for i := range 5 {
		require.NoError(t, hot.AddHotScore(ctx, int64(i+1), HotActionLike, now.Add(-time.Minute)))
	}

	first, err := hot.Page(ctx, now, nil, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, int64(5), first[0].VideoID, "同分按 video_id 降序")
	require.Equal(t, int64(4), first[1].VideoID)

	cursor := &HotCursor{WindowEnd: now, HotScore: first[1].HotScore, VideoID: first[1].VideoID}
	second, err := hot.Page(ctx, now, cursor, 10)
	require.NoError(t, err)
	require.Len(t, second, 3)
	require.Equal(t, int64(3), second[0].VideoID)
	require.Equal(t, int64(1), second[2].VideoID)
}

// TestHotWindowHardCap：窗口条目有硬上限（防止 Redis 结构无限膨胀）。
func TestHotWindowHardCap(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

	total := hotWindowMaxEntries + 50
	pipe := client.Pipeline()
	for i := range total {
		pipe.ZIncrBy(ctx, hotMinuteKey(now), float64(i+1), hotRankMember(int64(i+1)))
	}
	_, err := pipe.Exec(ctx)
	require.NoError(t, err)

	size, err := hot.MaterializeWindow(ctx, now)
	require.NoError(t, err)
	require.Equal(t, hotWindowMaxEntries, size, "窗口必须被裁剪到硬上限")

	// 保留的是分数最高的那批
	entries, err := hot.Page(ctx, now, nil, 1)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(total), entries[0].VideoID, "分数最高的条目应保留")
}

// TestHotPageEmptyWindowReportsUnavailable：窗口为空（近期无互动）时
// 返回哨兵错误，调用方据此降级为时间流，而不是把"没热度"当成故障。
func TestHotPageEmptyWindowReportsUnavailable(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)

	_, err := hot.Page(context.Background(), time.Now(), nil, 10)
	require.ErrorIs(t, err, ErrHotFeedUnavailable)
}

// TestHotWorkerMaterializesWindow：后台物化让读路径只读不算。
func TestHotWorkerMaterializesWindow(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)
	ctx := context.Background()
	now := hot.CurrentWindowEnd()

	require.NoError(t, hot.AddHotScore(ctx, 7, HotActionLike, now))
	worker := NewHotWindowWorker(hot, time.Hour)
	worker.materializeOnce(ctx)

	exists, err := client.Exists(ctx, hotWindowKey(now)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "后台物化后窗口必须已存在")

	entries, err := hot.Page(ctx, now, nil, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(7), entries[0].VideoID)
}

var _ = fmt.Sprintf

func TestHotPagesTraverseLargeTieGroupAcrossDigitBoundaries(t *testing.T) {
	client := newHotTestClient(t)
	hot := NewRedisHotFeed(client, nil)
	ctx := context.Background()
	now := hot.CurrentWindowEnd()
	for id := int64(1); id <= 120; id++ {
		require.NoError(t, hot.AddHotScore(ctx, id, HotActionLike, now))
	}
	var cursor *HotCursor
	var ids []int64
	for {
		page, err := hot.Page(ctx, now, cursor, 3)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, entry := range page {
			ids = append(ids, entry.VideoID)
		}
		last := page[len(page)-1]
		cursor = &HotCursor{WindowEnd: now, HotScore: last.HotScore, VideoID: last.VideoID}
		require.LessOrEqual(t, len(ids), 120, "pagination must terminate without duplicates")
	}
	expected := make([]int64, 120)
	for i := range expected {
		expected[i] = int64(120 - i)
	}
	require.Equal(t, expected, ids)
}
