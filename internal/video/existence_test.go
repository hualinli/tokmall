package video

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeGuard 记录缓存穿透防护的调用，用来断言"该挡的挡住、不该写的别写"。
type fakeGuard struct {
	// reject 为 true 时表示负缓存判定"一定不存在"。
	reject bool
	err    error

	mayExistCalls int
	markedMissing []int64
}

func (g *fakeGuard) MayExist(context.Context, string, int64) (bool, error) {
	g.mayExistCalls++
	if g.err != nil {
		return true, g.err
	}
	return !g.reject, nil
}

func (g *fakeGuard) MarkMissing(_ context.Context, _ string, id int64) {
	g.markedMissing = append(g.markedMissing, id)
}

// TestGetShortCircuitsWhenGuardRejects：负缓存说"不存在"时，
// 详情查询不应触达仓储层（这正是防护的意义：把穿透请求挡在数据库之外）。
func TestGetShortCircuitsWhenGuardRejects(t *testing.T) {
	repo := newFakeRepo()
	guard := &fakeGuard{reject: true}
	svc := newTestService(repo, newFakeStorage())
	svc.SetExistenceGuard(guard)

	_, err := svc.Get(context.Background(), 1, 12345)
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, 1, guard.mayExistCalls)
	require.Zero(t, repo.findCalls, "被防护层拒绝时不应查询数据库")
}

// TestGetMarksMissingOnlyWhenRecordAbsent：只有"记录确实不存在"才回写负缓存。
//
// 这是本功能最容易写错的地方：未发布视频对非作者同样返回 ErrNotFound，
// 如果把它也写进负缓存，作者本人在 TTL 窗口内会被自己的视频 404。
func TestGetMarksMissingOnlyWhenRecordAbsent(t *testing.T) {
	t.Run("record absent", func(t *testing.T) {
		repo := newFakeRepo()
		guard := &fakeGuard{}
		svc := newTestService(repo, newFakeStorage())
		svc.SetExistenceGuard(guard)

		_, err := svc.Get(context.Background(), 1, 999)
		require.ErrorIs(t, err, ErrNotFound)
		require.Equal(t, []int64{999}, guard.markedMissing, "记录不存在应回写负缓存")
	})

	t.Run("visibility denied to non-author", func(t *testing.T) {
		repo := newFakeRepo()
		pending := &Video{ID: 7, AuthorID: 42, Status: StatusPendingReview, Title: "t"}
		repo.videos[7] = pending
		guard := &fakeGuard{}
		svc := newTestService(repo, newFakeStorage())
		svc.SetExistenceGuard(guard)

		_, err := svc.Get(context.Background(), 99, 7) // 99 不是作者
		require.ErrorIs(t, err, ErrNotFound)
		require.Empty(t, guard.markedMissing,
			"可见性拒绝不能写负缓存，否则作者本人在 TTL 内也看不到自己的视频")
	})

	t.Run("author sees own pending video", func(t *testing.T) {
		repo := newFakeRepo()
		repo.videos[7] = &Video{ID: 7, AuthorID: 42, Status: StatusPendingReview, Title: "t", ObjectKey: "videos/42/a.mp4"}
		guard := &fakeGuard{}
		store := newFakeStorage()
		svc := newTestService(repo, store)
		svc.SetExistenceGuard(guard)

		detail, err := svc.Get(context.Background(), 42, 7)
		require.NoError(t, err)
		require.Equal(t, int64(7), detail.VideoID)
		require.Empty(t, guard.markedMissing)
	})
}

// TestGuardFailureFailsOpen：防护层自身出错时必须放行到数据库，
// 缓存/过滤器故障不能把真实存在的数据判成不存在。
func TestGuardFailureFailsOpen(t *testing.T) {
	repo := newFakeRepo()
	repo.videos[3] = &Video{ID: 3, AuthorID: 1, Status: StatusPublished, Title: "t", ObjectKey: "videos/1/a.mp4"}
	guard := &fakeGuard{err: context.DeadlineExceeded}
	svc := newTestService(repo, newFakeStorage())
	svc.SetExistenceGuard(guard)

	detail, err := svc.Get(context.Background(), 1, 3)
	require.NoError(t, err)
	require.Equal(t, int64(3), detail.VideoID)
}

var _ = time.Second
