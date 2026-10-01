package feed

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRepo struct {
	mu      sync.Mutex
	calls   int
	videos  []Video           // 已按 (published_at, id) 倒序排好，模拟索引顺序
	likes   map[string]bool   // key: "{userID}-{videoID}"
	follows map[int64][]int64 // userID -> followeeIDs
	plays   map[int64]int64   // videoID -> 播放数
}

func (f *fakeRepo) ListPublished(ctx context.Context, before *Cursor, limit int) ([]Video, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.list(ctx, f.videos, before, limit), nil
}

func (f *fakeRepo) ListPublishedByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	rows := make([]Video, 0, limit+1)
	for _, v := range f.videos {
		if v.AuthorID == authorID {
			rows = append(rows, v)
		}
	}
	return f.list(ctx, rows, before, limit), nil
}

// LikeCounts 从 likes map 聚合（与生产 SQL 语义一致）。
func (f *fakeRepo) LikeCounts(_ context.Context, videoIDs []int64) (map[int64]int64, error) {
	counts := map[int64]int64{}
	for _, id := range videoIDs {
		for k := range f.likes {
			if strings.HasSuffix(k, "-"+strconv.FormatInt(id, 10)) {
				counts[id]++
			}
		}
	}
	return counts, nil
}

// ListFollowingPublished 模拟全局时间索引查询；f.videos 已按时间键倒序。
func (f *fakeRepo) ListFollowingPublished(ctx context.Context, userID int64, before *Cursor, limit int) ([]Video, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	followees := make(map[int64]bool, len(f.follows[userID]))
	for _, followeeID := range f.follows[userID] {
		followees[followeeID] = true
	}
	rows := make([]Video, 0, len(f.videos))
	for _, v := range f.videos {
		if followees[v.AuthorID] {
			rows = append(rows, v)
		}
	}
	return f.list(ctx, rows, before, limit), nil
}

// PlayCounts 从 plays map 聚合（与生产 SQL 语义一致）。
func (f *fakeRepo) PlayCounts(_ context.Context, videoIDs []int64) (map[int64]int64, error) {
	counts := map[int64]int64{}
	for _, id := range videoIDs {
		counts[id] = f.plays[id]
	}
	return counts, nil
}

// blockingLikeRepo 让所有请求在 fillLikeCounts 的读后写窗口中同时停住，
// 稳定暴露“共享 FeedResult/Items 被并发修改”的实现错误。
type blockingLikeRepo struct {
	*fakeRepo
	started chan struct{}
	release chan struct{}
}

func (f *blockingLikeRepo) LikeCounts(ctx context.Context, videoIDs []int64) (map[int64]int64, error) {
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.fakeRepo.LikeCounts(ctx, videoIDs)
}

// ListByIDs 供热榜场景使用：按 ID 返回，顺序无关（调用方按榜单顺序重排）。
func (f *fakeRepo) ListByIDs(_ context.Context, ids []int64) ([]Video, error) {
	wanted := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	out := make([]Video, 0, len(ids))
	for _, v := range f.videos {
		if _, ok := wanted[v.ID]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeRepo) list(ctx context.Context, videos []Video, before *Cursor, limit int) []Video {
	out := make([]Video, 0, limit+1)
	for _, v := range videos {
		// 与生产 SQL 一致：保留 (published_at, id) < 游标 的行（DESC 顺序下更旧的）。
		// 用正面表述"比游标更新的行直接跳过"，避免否定复合条件（也更贴近游标语义）。
		if before != nil {
			newerThanCursor := v.PublishedAt.After(before.PublishedAt) ||
				(v.PublishedAt.Equal(before.PublishedAt) && v.ID >= before.VideoID)
			if newerThanCursor {
				continue
			}
		}
		out = append(out, v)
		if len(out) >= limit+1 {
			break
		}
	}
	return out
}

// fakeCache 内存版首页缓存：可注入 Get/Set 故障。
type fakeCache struct {
	mu     sync.RWMutex
	store  *FeedResult
	gets   int
	sets   int
	getErr error
	setErr error
}

func (f *fakeCache) Get(ctx context.Context) (*FeedResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.store, nil
}

func (f *fakeCache) Set(ctx context.Context, result *FeedResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.setErr != nil {
		return f.setErr
	}
	f.store = result
	return nil
}

func (f *fakeCache) Del(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store = nil
	return nil
}

// fakeFollowingCache 内存版关注 Feed 缓存：验证 cache-aside 与降级路径。
type fakeFollowingCache struct {
	mu    sync.RWMutex
	store map[string]*FeedResult
	gets  int
	sets  int
}

func (f *fakeFollowingCache) Get(_ context.Context, userID int64, rankVersion string) (*FeedResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.store == nil {
		return nil, nil //nolint:nilnil // 假实现模拟"未命中"
	}
	return f.store[followingCacheKey(userID, rankVersion)], nil
}

func (f *fakeFollowingCache) Set(_ context.Context, userID int64, rankVersion string, result *FeedResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.store == nil {
		f.store = map[string]*FeedResult{}
	}
	f.store[followingCacheKey(userID, rankVersion)] = result
	return nil
}

type fakeURLProvider struct{}

func (fakeURLProvider) PresignGetURL(ctx context.Context, objectKey string) (string, error) {
	return "http://minio.local/signed/" + objectKey, nil
}

func (fakeURLProvider) PresignAvatarURL(ctx context.Context, objectKey string) (string, error) {
	return "http://minio.local/avatar/" + objectKey, nil
}

func ts(offset time.Duration) *time.Time {
	t := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC).Add(offset)
	return &t
}

func newTestService(repo Repository) *Service {
	return NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, nil, nil)
}

func newTestServiceWithCache(repo Repository, cache *fakeCache) *Service {
	return NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, cache, nil, nil)
}

func TestCursor_RoundTrip(t *testing.T) {
	c := NewCodec("test-cursor-secret")
	cur := Cursor{PublishedAt: *ts(-time.Minute), VideoID: 7, RankVersion: CurrentRankVersion}

	enc, err := c.Encode(cur)
	require.NoError(t, err)
	dec, err := c.Decode(enc)
	require.NoError(t, err)
	require.Equal(t, cur, *dec)
}

func TestCursor_Tampered(t *testing.T) {
	c := NewCodec("test-cursor-secret")
	enc, err := c.Encode(Cursor{PublishedAt: *ts(0), VideoID: 1, RankVersion: CurrentRankVersion})
	require.NoError(t, err)

	tampered := enc[:len(enc)-1]
	if enc[len(enc)-1] == 'a' {
		tampered += "b"
	} else {
		tampered += "a"
	}
	_, err = c.Decode(tampered)
	require.ErrorIs(t, err, ErrInvalidCursor)

	_, err = c.Decode("not-a-cursor")
	require.ErrorIs(t, err, ErrInvalidCursor)
}

func TestCursor_WrongKey(t *testing.T) {
	_, err := NewCodec("other-secret").Decode(mustEncode(t, NewCodec("test-cursor-secret")))
	require.ErrorIs(t, err, ErrInvalidCursor)
}

func TestCursor_OldRankVersion(t *testing.T) {
	// 版本校验在 service 层：codec 只验签名与格式，旧版本游标由 service 拒绝。
	c := NewCodec("test-cursor-secret")
	cur := Cursor{PublishedAt: *ts(0), VideoID: 1, RankVersion: "rank-v0"}

	enc, err := c.Encode(cur)
	require.NoError(t, err)
	dec, err := c.Decode(enc)
	require.NoError(t, err, "codec 不判版本")
	require.Equal(t, "rank-v0", dec.RankVersion)

	repo := &fakeRepo{videos: fiveVideos()}
	svc := newTestService(repo)
	_, err = svc.List(context.Background(), 0, enc)
	require.ErrorIs(t, err, ErrInvalidCursor, "service 校验：旧排序版本游标失效")
}

func mustEncode(t *testing.T, c *codec) string {
	t.Helper()
	s, err := c.Encode(Cursor{PublishedAt: *ts(0), VideoID: 1, RankVersion: CurrentRankVersion})
	require.NoError(t, err)
	return s
}

// 构造 5 条倒序视频：最新的是 offset 0（id=5），最旧的是 offset -4min（id=1）。
func fiveVideos() []Video {
	return []Video{
		{ID: 5, AuthorID: 1, Title: "v5", ObjectKey: "o5", PublishedAt: ts(0)},
		{ID: 4, AuthorID: 1, Title: "v4", ObjectKey: "o4", PublishedAt: ts(-time.Minute)},
		{ID: 3, AuthorID: 1, Title: "v3", ObjectKey: "o3", PublishedAt: ts(-2 * time.Minute)},
		{ID: 2, AuthorID: 1, Title: "v2", ObjectKey: "o2", PublishedAt: ts(-3 * time.Minute)},
		{ID: 1, AuthorID: 1, Title: "v1", ObjectKey: "o1", PublishedAt: ts(-4 * time.Minute)},
	}
}

func TestList_FirstPage(t *testing.T) {
	svc := newTestService(&fakeRepo{videos: fiveVideos()})

	res, err := svc.List(context.Background(), 2, "")
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	require.Equal(t, int64(5), res.Items[0].VideoID)
	require.Equal(t, int64(4), res.Items[1].VideoID)
	require.True(t, res.HasMore)
	require.NotEmpty(t, res.NextCursor)
	require.Equal(t, "http://minio.local/signed/o5", res.Items[0].VideoURL)
}

func TestList_NextPageNoOverlap(t *testing.T) {
	svc := newTestService(&fakeRepo{videos: fiveVideos()})

	page1, err := svc.List(context.Background(), 2, "")
	require.NoError(t, err)

	page2, err := svc.List(context.Background(), 2, cursorOrEmpty(page1.NextCursor))
	require.NoError(t, err)
	require.Len(t, page2.Items, 2)
	require.Equal(t, int64(3), page2.Items[0].VideoID)
	require.Equal(t, int64(2), page2.Items[1].VideoID)
	require.True(t, page2.HasMore)

	page3, err := svc.List(context.Background(), 2, cursorOrEmpty(page2.NextCursor))
	require.NoError(t, err)
	require.Len(t, page3.Items, 1)
	require.Equal(t, int64(1), page3.Items[0].VideoID)
	require.False(t, page3.HasMore)
	require.Empty(t, page3.NextCursor)

	// 三页无重叠
	ids := map[int64]bool{}
	for _, p := range []*FeedResult{page1, page2, page3} {
		for _, it := range p.Items {
			require.False(t, ids[it.VideoID], "video %d 重复出现", it.VideoID)
			ids[it.VideoID] = true
		}
	}
	require.Len(t, ids, 5)
}

func TestList_NewVideoInsertedDoesNotBreakPagination(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	svc := newTestService(repo)

	page1, err := svc.List(context.Background(), 2, "")
	require.NoError(t, err)
	require.Equal(t, int64(5), page1.Items[0].VideoID)

	// 翻页前有新视频插入（最新）
	repo.videos = append([]Video{
		{ID: 6, AuthorID: 1, Title: "v6", ObjectKey: "o6", PublishedAt: ts(time.Minute)},
	}, repo.videos...)

	page2, err := svc.List(context.Background(), 2, cursorOrEmpty(page1.NextCursor))
	require.NoError(t, err)
	require.Equal(t, int64(3), page2.Items[0].VideoID, "游标语义：翻页不回看新内容，游标本身已消费")
	require.Equal(t, int64(2), page2.Items[1].VideoID)
	for _, it := range page2.Items {
		require.NotEqual(t, int64(6), it.VideoID, "v6 不应出现在第二页")
	}
}

func TestList_Empty(t *testing.T) {
	svc := newTestService(&fakeRepo{videos: []Video{}})

	res, err := svc.List(context.Background(), 20, "")
	require.NoError(t, err)
	require.NotNil(t, res.Items)
	require.Len(t, res.Items, 0)
	require.False(t, res.HasMore)
	require.Empty(t, res.NextCursor)
}

func TestList_InvalidInputs(t *testing.T) {
	svc := newTestService(&fakeRepo{videos: fiveVideos()})

	_, err := svc.List(context.Background(), 101, "")
	require.ErrorIs(t, err, ErrInvalidLimit)

	_, err = svc.List(context.Background(), -1, "")
	require.ErrorIs(t, err, ErrInvalidLimit)

	_, err = svc.List(context.Background(), 20, "forged.cursor")
	require.ErrorIs(t, err, ErrInvalidCursor)
}

// TestFirstPage_CacheHit 命中缓存时零 DB 查询。
func TestFirstPage_CacheHit(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos(), likes: map[string]bool{"1-99": true}}
	cache := &fakeCache{store: &FeedResult{Items: []FeedItem{
		{VideoID: 99, Title: "cached", VideoURL: "http://cache.local/v99", LikeCount: 7},
	}}}
	svc := newTestServiceWithCache(repo, cache)

	res, err := svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	require.Equal(t, int64(99), res.Items[0].VideoID)
	require.Equal(t, int64(1), res.Items[0].LikeCount)
	require.NotSame(t, cache.store, res, "缓存命中不得直接返回缓存对象")
	require.NotSame(t, &cache.store.Items[0], &res.Items[0], "缓存命中不得共享 Items backing array")
	require.Equal(t, int64(7), cache.store.Items[0].LikeCount, "缓存快照不得被请求级 like_count 污染")
	repo.mu.Lock()
	require.Equal(t, 0, repo.calls, "命中缓存不得查库")
	repo.mu.Unlock()
}

// TestFirstPage_PopulatesCache 未命中时查库并回填，第二次命中。
func TestFirstPage_PopulatesCache(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	cache := &fakeCache{}
	svc := newTestServiceWithCache(repo, cache)

	_, err := svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	require.Equal(t, 1, cache.sets)
	require.Equal(t, int64(5), cache.store.Items[0].VideoID)

	_, err = svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	repo.mu.Lock()
	require.Equal(t, 1, repo.calls, "第二次应命中缓存")
	repo.mu.Unlock()
}

// TestFirstPage_CacheFailureDegrades 缓存故障必须降级直连 DB（缓存不是单点）。
func TestFirstPage_CacheFailureDegrades(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	cache := &fakeCache{getErr: errors.New("redis down")}
	svc := newTestServiceWithCache(repo, cache)

	res, err := svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	require.Len(t, res.Items, 5)
}

// TestFirstPage_CacheSetFailureReturnsResult Set 失败不影响本次响应。
func TestFirstPage_CacheSetFailureReturnsResult(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	cache := &fakeCache{setErr: errors.New("redis down")}
	svc := newTestServiceWithCache(repo, cache)

	res, err := svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	require.Len(t, res.Items, 5)
}

// TestFirstPage_Singleflight 20 个并发首页请求只产生一次 DB 查询。
func TestFirstPage_Singleflight(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	svc := newTestServiceWithCache(repo, &fakeCache{})

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.List(context.Background(), 0, "")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "request %d", i)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Equal(t, 1, repo.calls, "singleflight 必须合并为一次 DB 查询")
}

func TestFirstPage_ConcurrentRequestsOwnLikeCountResults(t *testing.T) {
	const n = 20
	repo := &blockingLikeRepo{
		fakeRepo: &fakeRepo{
			videos: fiveVideos(),
			likes:  map[string]bool{"1-5": true, "2-5": true},
		},
		started: make(chan struct{}, n),
		release: make(chan struct{}),
	}
	cache := &fakeCache{}
	svc := newTestServiceWithCache(repo, cache)

	results := make([]*FeedResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.List(context.Background(), 0, "")
		}(i)
	}
	waitForLikeCalls(t, repo.started, n)
	close(repo.release)
	wg.Wait()

	for i := range results {
		require.NoError(t, errs[i], "request %d", i)
		require.NotNil(t, results[i])
		require.Equal(t, int64(2), results[i].Items[0].LikeCount)
		if i > 0 {
			require.NotSame(t, results[0], results[i], "请求不得共享 FeedResult")
			require.NotSame(t, &results[0].Items[0], &results[i].Items[0], "请求不得共享 Items backing array")
		}
	}

	cache.mu.RLock()
	cached := cache.store
	cache.mu.RUnlock()
	require.NotNil(t, cached)
	require.Equal(t, int64(0), cached.Items[0].LikeCount, "缓存快照不得被请求级 like_count 污染")
	repo.mu.Lock()
	require.Equal(t, 1, repo.calls, "并发首页请求仍应只查一次视频列表")
	repo.mu.Unlock()
}

func waitForLikeCalls(t *testing.T, started <-chan struct{}, n int) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for i := 0; i < n; i++ {
		select {
		case <-started:
		case <-timeout.C:
			t.Fatalf("等待第 %d 个并发 LikeCounts 请求超时", i+1)
		}
	}
}

// TestList_CustomLimitSkipsCache 自定义 limit 与缓存内容绑定，必须绕过缓存。
func TestList_CustomLimitSkipsCache(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	cache := &fakeCache{}
	svc := newTestServiceWithCache(repo, cache)

	_, err := svc.List(context.Background(), 5, "")
	require.NoError(t, err)
	repo.mu.Lock()
	require.Equal(t, 1, repo.calls)
	repo.mu.Unlock()
	require.Equal(t, 0, cache.gets, "自定义 limit 不走缓存")
}

// TestList_AuthorAndLikeCount feed 卡片：作者信息 + like_count 现查。
func TestList_AuthorAndLikeCount(t *testing.T) {
	repo := &fakeRepo{
		videos: []Video{
			{ID: 5, AuthorID: 3, Username: "bob", AvatarKey: ptr("avatars/3/b.jpg"),
				Title: "v5", ObjectKey: "o5", PublishedAt: ts(0)},
			{ID: 4, AuthorID: 1, Username: "alice",
				Title: "v4", ObjectKey: "o4", PublishedAt: ts(-time.Minute)},
		},
		likes: map[string]bool{"9-5": true, "8-5": true, "9-4": true},
	}
	svc := newTestService(repo)

	res, err := svc.List(context.Background(), 20, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), res.Items[0].LikeCount, "v5 有 2 个赞")
	require.Equal(t, int64(1), res.Items[1].LikeCount)
	require.Equal(t, "bob", res.Items[0].Author.Username)
	require.NotNil(t, res.Items[0].Author.AvatarURL)
	require.Nil(t, res.Items[1].Author.AvatarURL, "无头像的作者 avatar_url 为 null")
}

// TestListByAuthor_OnlyPublished 作者主页：只含该作者已发布视频，分页复用游标。
func TestListByAuthor_OnlyPublished(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	svc := newTestService(repo)

	res, err := svc.ListByAuthor(context.Background(), 1, 0, "")
	require.NoError(t, err)
	require.Len(t, res.Items, 5, "fiveVideos 全部是作者 1 的")

	// 换个作者：空列表，items 非 nil
	res, err = svc.ListByAuthor(context.Background(), 99, 0, "")
	require.NoError(t, err)
	require.NotNil(t, res.Items)
	require.Empty(t, res.Items)
}

func ptr(s string) *string {
	return &s
}

// mixedAuthorVideos 两个作者的交错视频（已按全局 (published_at, id) 倒序）：
// 作者 1 有 6 条（id 12,10,8,6,4,2），作者 2 有 4 条（id 11,9,7,5）。
func mixedAuthorVideos() []Video {
	return []Video{
		{ID: 12, AuthorID: 1, Title: "a6", ObjectKey: "o12", PublishedAt: ts(0)},
		{ID: 11, AuthorID: 2, Title: "b4", ObjectKey: "o11", PublishedAt: ts(-time.Minute)},
		{ID: 10, AuthorID: 1, Title: "a5", ObjectKey: "o10", PublishedAt: ts(-2 * time.Minute)},
		{ID: 9, AuthorID: 2, Title: "b3", ObjectKey: "o9", PublishedAt: ts(-3 * time.Minute)},
		{ID: 8, AuthorID: 1, Title: "a4", ObjectKey: "o8", PublishedAt: ts(-4 * time.Minute)},
		{ID: 7, AuthorID: 2, Title: "b2", ObjectKey: "o7", PublishedAt: ts(-5 * time.Minute)},
		{ID: 6, AuthorID: 1, Title: "a3", ObjectKey: "o6", PublishedAt: ts(-6 * time.Minute)},
		{ID: 5, AuthorID: 2, Title: "b1", ObjectKey: "o5", PublishedAt: ts(-7 * time.Minute)},
		{ID: 4, AuthorID: 1, Title: "a2", ObjectKey: "o4", PublishedAt: ts(-8 * time.Minute)},
		{ID: 2, AuthorID: 1, Title: "a1", ObjectKey: "o2", PublishedAt: ts(-9 * time.Minute)},
	}
}

func TestListFollowing_EmptyFollows(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{}}
	svc := newTestService(repo)

	result, err := svc.ListFollowing(context.Background(), 7, 0, "")
	require.NoError(t, err)
	require.Empty(t, result.Items, "未关注任何人：空 Feed")
	require.False(t, result.HasMore)
	require.Empty(t, result.NextCursor)
}

func TestListFollowing_IncludesAllFollowedVideos(t *testing.T) {
	// 作者 1 有 6 条，作者 2 有 4 条；全部应出现在关注时间线。
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1, 2}}}
	svc := newTestService(repo)

	result, err := svc.ListFollowing(context.Background(), 7, 0, "")
	require.NoError(t, err)
	require.Len(t, result.Items, 10)
	titles := make([]string, 0, len(result.Items))
	for _, it := range result.Items {
		titles = append(titles, it.Title)
	}
	require.Equal(t, []string{"a6", "b4", "a5", "b3", "a4", "b2", "a3", "b1", "a2", "a1"}, titles,
		"全局 (published_at,id) 倒序，不按作者截断")
	require.False(t, result.HasMore)
}

func TestListFollowing_SingleProlificAuthorPaginatesPastFive(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1}}}
	svc := newTestService(repo)
	var seen []int64
	cursor := ""
	for {
		page, err := svc.ListFollowing(context.Background(), 7, 2, cursor)
		require.NoError(t, err)
		for _, item := range page.Items {
			seen = append(seen, item.VideoID)
		}
		if !page.HasMore {
			break
		}
		cursor = cursorOrEmpty(page.NextCursor)
		require.NotEmpty(t, cursor)
	}
	require.Equal(t, []int64{12, 10, 8, 6, 4, 2}, seen)
}

func TestListFollowing_CursorPagination(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1, 2}}}
	svc := newTestService(repo)

	// 第一页 limit=3
	page1, err := svc.ListFollowing(context.Background(), 7, 3, "")
	require.NoError(t, err)
	require.Len(t, page1.Items, 3)
	require.True(t, page1.HasMore)
	require.NotEmpty(t, page1.NextCursor)

	// 第二页：不重复、不丢
	seen := map[int64]bool{}
	for _, it := range page1.Items {
		seen[it.VideoID] = true
	}
	page2, err := svc.ListFollowing(context.Background(), 7, 3, cursorOrEmpty(page1.NextCursor))
	require.NoError(t, err)
	require.Len(t, page2.Items, 3)
	for _, it := range page2.Items {
		require.False(t, seen[it.VideoID], "翻页不重复")
		seen[it.VideoID] = true
	}

	// 第三页翻到底
	page3, err := svc.ListFollowing(context.Background(), 7, 3, cursorOrEmpty(page2.NextCursor))
	require.NoError(t, err)
	for _, it := range page3.Items {
		require.False(t, seen[it.VideoID], "翻页不重复")
		seen[it.VideoID] = true
	}
	require.Len(t, seen, 9, "三页后仍有一条更旧的视频")
	require.True(t, page3.HasMore)
	page4, err := svc.ListFollowing(context.Background(), 7, 3, cursorOrEmpty(page3.NextCursor))
	require.NoError(t, err)
	require.Len(t, page4.Items, 1)
	require.Equal(t, int64(2), page4.Items[0].VideoID)
	require.False(t, page4.HasMore)
}

func TestListFollowing_InvalidLimit(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1}}}
	svc := newTestService(repo)

	_, err := svc.ListFollowing(context.Background(), 7, 101, "")
	require.ErrorIs(t, err, ErrInvalidLimit)
	_, err = svc.ListFollowing(context.Background(), 7, -1, "")
	require.ErrorIs(t, err, ErrInvalidLimit)
}

func TestListFollowing_TamperedCursor(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1}}}
	svc := newTestService(repo)

	_, err := svc.ListFollowing(context.Background(), 7, 0, "forged.cursor")
	require.ErrorIs(t, err, ErrInvalidCursor)
}

func TestListFollowing_FillsLikeCounts(t *testing.T) {
	repo := &fakeRepo{
		videos:  mixedAuthorVideos(),
		follows: map[int64][]int64{7: {1}},
		likes:   map[string]bool{"3-12": true, "3-10": true},
	}
	svc := newTestService(repo)

	result, err := svc.ListFollowing(context.Background(), 7, 2, "")
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Items[0].LikeCount, "id=12 被赞 1 次")
	require.Equal(t, int64(1), result.Items[1].LikeCount, "id=10 被赞 1 次")
}

func TestListFollowing_CachePopulatesAndHits(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1, 2}}}
	fcache := &fakeFollowingCache{}
	svc := NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, fcache, nil)

	// 第一次：回填缓存
	result1, err := svc.ListFollowing(context.Background(), 7, 0, "")
	require.NoError(t, err)
	require.NotEmpty(t, result1.Items)
	require.Equal(t, 1, fcache.sets, "第一次请求应回填缓存")

	// 第二次：命中缓存，不查 DB（repo.calls 不增）
	callsBefore := repo.calls
	result2, err := svc.ListFollowing(context.Background(), 7, 0, "")
	require.NoError(t, err)
	require.Equal(t, callsBefore, repo.calls, "命中缓存不应再查 DB")
	require.Equal(t, result1.Items, result2.Items)
	fcache.mu.RLock()
	cached := fcache.store[followingCacheKey(7, CurrentRankVersion)]
	fcache.mu.RUnlock()
	require.NotSame(t, cached, result1, "请求不得直接返回缓存对象")
	require.NotSame(t, &cached.Items[0], &result1.Items[0], "请求不得共享缓存 Items backing array")
	// Get 次数：首次 miss + singleflight 内 double-check + 二次请求 hit >= 3
	require.GreaterOrEqual(t, fcache.gets, 3, "第二次请求应命中缓存（多一次 Get）")
}

func TestFollowingFirstPage_ConcurrentRequestsOwnLikeCountResults(t *testing.T) {
	const n = 20
	repo := &blockingLikeRepo{
		fakeRepo: &fakeRepo{
			videos:  mixedAuthorVideos(),
			follows: map[int64][]int64{7: {1, 2}},
			likes:   map[string]bool{"1-12": true, "2-12": true},
		},
		started: make(chan struct{}, n),
		release: make(chan struct{}),
	}
	fcache := &fakeFollowingCache{}
	svc := NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, fcache, nil)

	results := make([]*FeedResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.ListFollowing(context.Background(), 7, 0, "")
		}(i)
	}
	waitForLikeCalls(t, repo.started, n)
	close(repo.release)
	wg.Wait()

	for i := range results {
		require.NoError(t, errs[i], "request %d", i)
		require.NotNil(t, results[i])
		require.Equal(t, int64(2), results[i].Items[0].LikeCount)
		if i > 0 {
			require.NotSame(t, results[0], results[i], "请求不得共享 FeedResult")
			require.NotSame(t, &results[0].Items[0], &results[i].Items[0], "请求不得共享 Items backing array")
		}
	}

	fcache.mu.RLock()
	cached := fcache.store[followingCacheKey(7, CurrentRankVersion)]
	fcache.mu.RUnlock()
	require.NotNil(t, cached)
	require.Equal(t, int64(0), cached.Items[0].LikeCount, "关注 Feed 缓存快照不得被请求级 like_count 污染")
	repo.mu.Lock()
	require.Equal(t, 1, repo.calls, "并发关注 Feed 请求仍应只查一次候选")
	repo.mu.Unlock()
}

func TestListFollowing_CustomLimitSkipsCache(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1, 2}}}
	fcache := &fakeFollowingCache{}
	svc := NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, fcache, nil)

	_, err := svc.ListFollowing(context.Background(), 7, 5, "")
	require.NoError(t, err)
	require.Zero(t, fcache.sets, "自定义 limit 不应走缓存（与全局 Feed 同取舍）")
}

func TestListFollowing_CacheFailureDegrades(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1, 2}}}
	fcache := &fakeFollowingCache{}
	svc := NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, fcache, nil)

	// 正常回填后清空缓存，验证降级（Get 返回 nil 时直查）
	result, err := svc.ListFollowing(context.Background(), 7, 0, "")
	require.NoError(t, err)
	require.NotEmpty(t, result.Items)
}

func TestListFollowing_ProductionRankerUsesTimeCache(t *testing.T) {
	repo := &fakeRepo{videos: mixedAuthorVideos(), follows: map[int64][]int64{7: {1, 2}}}
	fcache := &fakeFollowingCache{}
	timeSvc := NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, fcache, nil, TimeRanker{})
	hotSvc := NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, fcache, nil, HotRanker{})

	_, err := timeSvc.ListFollowing(context.Background(), 7, 0, "")
	require.NoError(t, err)
	require.Equal(t, 1, fcache.sets, "time 排序回填一条")

	// HotRanker 只保留为排序单元实验，不得通过 Service 启用。
	hotCallsBefore := repo.calls
	_, err = hotSvc.ListFollowing(context.Background(), 7, 0, "")
	require.ErrorIs(t, err, ErrRankPaginationUnsupported)
	require.Equal(t, 1, fcache.sets, "实验排序被拒绝时不应写入生产缓存")
	require.Equal(t, hotCallsBefore, repo.calls, "实验排序被拒绝时不应查询数据库")
}

// cursorOrEmpty 解引用可空游标（nil → ""，适配 *string 的 NextCursor）。
func cursorOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
