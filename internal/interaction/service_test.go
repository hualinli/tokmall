package interaction

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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

// fakeAvatar 预签名 URL 假实现。
type fakeAvatar struct{}

func (fakeAvatar) PresignGetURL(_ context.Context, objectKey string) (string, error) {
	return "http://minio.local/signed/" + objectKey, nil
}

// ListFollowing/ListFollowers 从 follows map 构造 FollowEdge（按 follower/followee 过滤）。
func (f *fakeRepo) ListFollowing(ctx context.Context, userID int64, before *Cursor, limit int) ([]FollowEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edges := make([]FollowEdge, 0)
	for k := range f.follows {
		if k[0] == userID {
			edges = append(edges, FollowEdge{UserID: k[1], CreatedAt: time.Now(), Username: fmt.Sprintf("u%d", k[1])})
		}
	}
	return f.filterEdges(edges, before, limit), nil
}

func (f *fakeRepo) ListFollowers(ctx context.Context, userID int64, before *Cursor, limit int) ([]FollowEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edges := make([]FollowEdge, 0)
	for k := range f.follows {
		if k[1] == userID {
			edges = append(edges, FollowEdge{UserID: k[0], CreatedAt: time.Now(), Username: fmt.Sprintf("u%d", k[0])})
		}
	}
	return f.filterEdges(edges, before, limit), nil
}

func (f *fakeRepo) filterEdges(edges []FollowEdge, before *Cursor, limit int) []FollowEdge {
	out := make([]FollowEdge, 0, limit+1)
	for _, e := range edges {
		if before != nil && !(e.CreatedAt.Before(before.CreatedAt) ||
			(e.CreatedAt.Equal(before.CreatedAt) && e.UserID < before.UserID)) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit+1 {
			break
		}
	}
	return out
}

func (f *fakeRepo) IsFollowingMany(ctx context.Context, viewerID int64, userIDs []int64) (map[int64]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[int64]bool, len(userIDs))
	if viewerID <= 0 {
		return result, nil
	}
	for _, id := range userIDs {
		result[id] = f.follows[[2]int64{viewerID, id}]
	}
	return result, nil
}

func newTestService(repo *fakeRepo) *Service {
	return NewService(repo, fakeAvatar{}, "test-cursor-secret")
}

func TestLike_RepeatCallsIdempotent(t *testing.T) {
	svc := newTestService(newFakeRepo())

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
	svc := newTestService(repo)

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
	svc := newTestService(newFakeRepo())

	_, err := svc.Follow(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrInvalidParam)
}

func TestFollow_FolloweeNotFound(t *testing.T) {
	svc := newTestService(newFakeRepo())

	_, err := svc.Follow(context.Background(), 1, 999)
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestFollow_RepeatAndUnfollow(t *testing.T) {
	svc := newTestService(newFakeRepo())

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

// TestListFollowing_Visibility 关注列表：匿名 is_following 全 false，
// 登录 viewer 批量回填；is_me 正确标记。
func TestListFollowing_Visibility(t *testing.T) {
	repo := newFakeRepo()
	repo.follows[[2]int64{1, 2}] = true
	repo.follows[[2]int64{1, 3}] = true
	repo.follows[[2]int64{1, 4}] = true
	svc := newTestService(repo)

	t.Run("anonymous", func(t *testing.T) {
		res, err := svc.ListFollowing(context.Background(), 0, 1, 20, "")
		require.NoError(t, err)
		require.Len(t, res.Items, 3)
		for _, it := range res.Items {
			require.False(t, it.IsFollowing, "匿名 is_following 必须全 false")
			require.False(t, it.IsMe)
		}
	})

	t.Run("viewer follows one of them", func(t *testing.T) {
		repo.follows[[2]int64{5, 2}] = true
		res, err := svc.ListFollowing(context.Background(), 5, 1, 20, "")
		require.NoError(t, err)
		found := map[int64]bool{}
		for _, it := range res.Items {
			found[it.UserID] = it.IsFollowing
		}
		require.True(t, found[2], "viewer 关注了 u2")
		require.False(t, found[3])
	})

	t.Run("own entry in others' follower list marks is_me", func(t *testing.T) {
		// viewer=1 关注了 u2；查看 u2 的粉丝列表时，u1 是自己 → is_me=true
		res, err := svc.ListFollowers(context.Background(), 1, 2, 20, "")
		require.NoError(t, err)
		for _, it := range res.Items {
			if it.UserID == 1 {
				require.True(t, it.IsMe)
				return
			}
		}
		require.Fail(t, "u1 应该在 u2 的粉丝列表里")
	})
}

// TestListFollowers_Basic 粉丝列表 + 分页参数校验。
func TestListFollowers_Basic(t *testing.T) {
	repo := newFakeRepo()
	repo.follows[[2]int64{2, 1}] = true
	repo.follows[[2]int64{3, 1}] = true
	svc := newTestService(repo)

	res, err := svc.ListFollowers(context.Background(), 0, 1, 20, "")
	require.NoError(t, err)
	require.Len(t, res.Items, 2)

	_, err = svc.ListFollowers(context.Background(), 0, 1, 101, "")
	require.ErrorIs(t, err, ErrInvalidParam)

	_, err = svc.ListFollowers(context.Background(), 0, 1, 20, "forged.cursor")
	require.ErrorIs(t, err, ErrInvalidCursor)
}

// TestCursor_RoundTrip 关注列表游标：签名 + 版本校验。
func TestCursor_RoundTrip(t *testing.T) {
	codec := NewCodec("test-cursor-secret")
	cur := Cursor{CreatedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), UserID: 7, RankVersion: CurrentRankVersion}

	enc, err := codec.Encode(cur)
	require.NoError(t, err)
	dec, err := codec.Decode(enc)
	require.NoError(t, err)
	require.Equal(t, cur, *dec)

	_, err = codec.Decode("forged.cursor")
	require.ErrorIs(t, err, ErrInvalidCursor)

	old := cur
	old.RankVersion = "follow-v0"
	enc, err = codec.Encode(old)
	require.NoError(t, err)
	_, err = codec.Decode(enc)
	require.ErrorIs(t, err, ErrInvalidCursor)
}
