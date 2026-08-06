package interaction

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRepo struct {
	mu      sync.Mutex
	likes   map[[2]int64]bool
	follows map[[2]int64]bool
	users   map[int64]bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		likes:   make(map[[2]int64]bool),
		follows: make(map[[2]int64]bool),
		users:   map[int64]bool{1: true, 4: true},
	}
}

// 幂等语义用 map 模拟：重复写入不报错。
func (f *fakeRepo) AddLike(ctx context.Context, userID, videoID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.likes[[2]int64{userID, videoID}] = true
	return nil
}

func (f *fakeRepo) RemoveLike(ctx context.Context, userID, videoID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.likes, [2]int64{userID, videoID})
	return nil
}

func (f *fakeRepo) AddFollow(ctx context.Context, followerID, followeeID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.follows[[2]int64{followerID, followeeID}] = true
	return nil
}

func (f *fakeRepo) RemoveFollow(ctx context.Context, followerID, followeeID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.follows, [2]int64{followerID, followeeID})
	return nil
}

func (f *fakeRepo) UserExists(ctx context.Context, userID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[userID], nil
}

func TestLike_RepeatCallsIdempotent(t *testing.T) {
	svc := NewService(newFakeRepo())

	for i := 0; i < 3; i++ {
		res, err := svc.Like(context.Background(), 1, 100)
		require.NoError(t, err)
		require.True(t, res.Liked)
	}
	// 之后取消
	res, err := svc.Unlike(context.Background(), 1, 100)
	require.NoError(t, err)
	require.False(t, res.Liked)
	// 重复取消也幂等
	res, err = svc.Unlike(context.Background(), 1, 100)
	require.NoError(t, err)
	require.False(t, res.Liked)
}

func TestLike_ConcurrentCallsNoError(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Like(context.Background(), 1, 100)
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.True(t, repo.likes[[2]int64{1, 100}], "并发点赞后关系应存在")
}

func TestFollow_SelfFollowRejected(t *testing.T) {
	svc := NewService(newFakeRepo())

	_, err := svc.Follow(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrInvalidParam)
}

func TestFollow_FolloweeNotFound(t *testing.T) {
	svc := NewService(newFakeRepo())

	_, err := svc.Follow(context.Background(), 1, 999)
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestFollow_RepeatAndUnfollow(t *testing.T) {
	svc := NewService(newFakeRepo())

	for i := 0; i < 3; i++ {
		res, err := svc.Follow(context.Background(), 1, 4)
		require.NoError(t, err)
		require.True(t, res.Following)
	}
	res, err := svc.Unfollow(context.Background(), 1, 4)
	require.NoError(t, err)
	require.False(t, res.Following)

	// 取消后可以重新关注（关系重新建立）
	res, err = svc.Follow(context.Background(), 1, 4)
	require.NoError(t, err)
	require.True(t, res.Following)
}
