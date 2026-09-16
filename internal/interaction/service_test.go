package interaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRepo struct {
	mu            sync.Mutex
	likes         map[[2]int64]bool
	follows       map[[2]int64]bool
	users         map[int64]bool
	deletedUsers  map[int64]bool
	visibleVideos map[int64]bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		likes:   make(map[[2]int64]bool),
		follows: make(map[[2]int64]bool),
		// 列表测试会使用 1~5 作为关系两端；999 仍作为不存在用户。
		users:         map[int64]bool{1: true, 2: true, 3: true, 4: true, 5: true},
		deletedUsers:  make(map[int64]bool),
		visibleVideos: map[int64]bool{100: true},
	}
}

func (f *fakeRepo) VideoIsInteractable(ctx context.Context, videoID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visibleVideos[videoID], nil
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
	return f.users[userID] && !f.deletedUsers[userID], nil
}

// fakeAvatar 预签名 URL 假实现。
type fakeAvatar struct{}

func (fakeAvatar) PresignAvatarURL(_ context.Context, objectKey string) (string, error) {
	return "http://minio.local/signed/" + objectKey, nil
}

type fakeFollowingInvalidator struct {
	mu    sync.Mutex
	calls []int64
	err   error
}

func (f *fakeFollowingInvalidator) InvalidateFollowingFeed(_ context.Context, followerID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, followerID)
	return f.err
}

func (f *fakeFollowingInvalidator) calledIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.calls...)
}

// ListFollowing/ListFollowers 从 follows map 构造 FollowEdge（按 follower/followee 过滤）。
func (f *fakeRepo) ListFollowing(ctx context.Context, userID int64, before *Cursor, limit int) ([]FollowEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edges := make([]FollowEdge, 0)
	for k := range f.follows {
		if k[0] == userID && !f.deletedUsers[k[1]] {
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
		if k[1] == userID && !f.deletedUsers[k[0]] {
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
		if !f.deletedUsers[id] {
			result[id] = f.follows[[2]int64{viewerID, id}]
		}
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

func TestLike_InvisibleVideoRejected(t *testing.T) {
	repo := newFakeRepo()
	// 101/102/103 分别代表未发布、已删除、不存在；read model 对外都返回 false。
	repo.visibleVideos[101] = false
	repo.visibleVideos[102] = false
	repo.likes[[2]int64{1, 102}] = true
	svc := newTestService(repo)

	for _, videoID := range []int64{101, 102, 103} {
		_, err := svc.Like(context.Background(), 1, videoID)
		require.ErrorIs(t, err, ErrVideoNotFound, "不可见视频不能点赞: video_id=%d", videoID)

		_, err = svc.Unlike(context.Background(), 1, videoID)
		require.ErrorIs(t, err, ErrVideoNotFound, "不可见视频不能取消点赞: video_id=%d", videoID)
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	// 预存的隐藏视频点赞不能被 unlike 请求删除；服务层必须先完成可见性检查。
	require.True(t, repo.likes[[2]int64{1, 102}])
	require.False(t, repo.likes[[2]int64{1, 101}])
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

func TestFollow_DeletedFolloweeTreatedAsNotFound(t *testing.T) {
	repo := newFakeRepo()
	repo.deletedUsers[4] = true
	svc := newTestService(repo)

	_, err := svc.Follow(context.Background(), 1, 4)
	require.ErrorIs(t, err, ErrUserNotFound)
	_, err = svc.Unfollow(context.Background(), 1, 4)
	require.ErrorIs(t, err, ErrUserNotFound)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.False(t, repo.follows[[2]int64{1, 4}], "不可见目标不能建立关注关系")
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

func TestFollow_InvalidatesFollowerFeedAfterSuccessfulWrites(t *testing.T) {
	repo := newFakeRepo()
	invalidator := &fakeFollowingInvalidator{}
	svc := NewService(repo, fakeAvatar{}, "test-cursor-secret", invalidator)

	// 关注关系改变的是 follower=1 看到的关注 Feed；不能误删 followee=4 的缓存。
	_, err := svc.Follow(context.Background(), 1, 4)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, invalidator.calledIDs())

	// 关系表的幂等重复也返回成功；重新失效是安全的，避免实现依赖“是否真的插入”
	// 这个额外结果，且不会改变对外幂等语义。
	_, err = svc.Follow(context.Background(), 1, 4)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 1}, invalidator.calledIDs())

	_, err = svc.Unfollow(context.Background(), 1, 4)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 1, 1}, invalidator.calledIDs())

	// 重复取消同样成功并失效同一个 follower 的第一页。
	_, err = svc.Unfollow(context.Background(), 1, 4)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 1, 1, 1}, invalidator.calledIDs())
}

func TestFollow_InvalidationFailureDoesNotFailSuccessfulWrite(t *testing.T) {
	repo := newFakeRepo()
	invalidator := &fakeFollowingInvalidator{err: errors.New("redis unavailable")}
	svc := NewService(repo, fakeAvatar{}, "test-cursor-secret", invalidator)

	result, err := svc.Follow(context.Background(), 1, 4)
	require.NoError(t, err, "缓存失效失败不能回滚已经成功的关注写入")
	require.True(t, result.Following)
	require.Equal(t, []int64{1}, invalidator.calledIDs())

	repo.mu.Lock()
	require.True(t, repo.follows[[2]int64{1, 4}], "数据库写入应保留，等待 TTL 收敛缓存")
	repo.mu.Unlock()
}

func TestFollow_CacheDisabledIsNilSafe(t *testing.T) {
	// NewService 的可选 invalidator 为 nil 时，关注路径仍正常工作。
	result, err := newTestService(newFakeRepo()).Follow(context.Background(), 1, 4)
	require.NoError(t, err)
	require.True(t, result.Following)
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

func TestFollowListsExcludeDeletedUsers(t *testing.T) {
	repo := newFakeRepo()
	repo.follows[[2]int64{1, 2}] = true
	repo.follows[[2]int64{1, 3}] = true
	repo.follows[[2]int64{2, 1}] = true
	repo.follows[[2]int64{3, 1}] = true
	repo.deletedUsers[3] = true
	svc := newTestService(repo)

	following, err := svc.ListFollowing(context.Background(), 0, 1, 20, "")
	require.NoError(t, err)
	require.Equal(t, []int64{2}, profileIDs(following.Items))

	followers, err := svc.ListFollowers(context.Background(), 0, 1, 20, "")
	require.NoError(t, err)
	require.Equal(t, []int64{2}, profileIDs(followers.Items))

	_, err = svc.ListFollowing(context.Background(), 0, 3, 20, "")
	require.ErrorIs(t, err, ErrUserNotFound, "已删除用户的列表资源按不存在处理")
}

func profileIDs(items []ProfileItem) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.UserID)
	}
	return ids
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
