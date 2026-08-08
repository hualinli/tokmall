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
	mu     sync.Mutex
	calls  int
	videos []Video // 已按 (published_at, id) 倒序排好，模拟索引顺序
	likes  map[string]bool // key: "{userID}-{videoID}"
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

func (f *fakeRepo) list(ctx context.Context, videos []Video, before *Cursor, limit int) []Video {
	out := make([]Video, 0, limit+1)
	for _, v := range videos {
		// 与生产 SQL 一致：保留 (published_at, id) < 游标 的行（DESC 顺序下更旧的）
		if before != nil && !(v.PublishedAt.Before(before.PublishedAt) ||
			(v.PublishedAt.Equal(before.PublishedAt) && v.ID < before.VideoID)) {
			continue
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
	store  *FeedResult
	gets   int
	sets   int
	getErr error
	setErr error
}

func (f *fakeCache) Get(ctx context.Context) (*FeedResult, error) {
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.store, nil
}

func (f *fakeCache) Set(ctx context.Context, result *FeedResult) error {
	f.sets++
	if f.setErr != nil {
		return f.setErr
	}
	f.store = result
	return nil
}

func (f *fakeCache) Del(ctx context.Context) error {
	f.store = nil
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

func newTestService(repo *fakeRepo) *Service {
	return NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, nil)
}

func newTestServiceWithCache(repo *fakeRepo, cache *fakeCache) *Service {
	return NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, cache, nil)
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
	c := NewCodec("test-cursor-secret")
	cur := Cursor{PublishedAt: *ts(0), VideoID: 1, RankVersion: "rank-v0"}

	enc, err := c.Encode(cur)
	require.NoError(t, err)
	_, err = c.Decode(enc)
	require.ErrorIs(t, err, ErrInvalidCursor)
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

	page2, err := svc.List(context.Background(), 2, page1.NextCursor)
	require.NoError(t, err)
	require.Len(t, page2.Items, 2)
	require.Equal(t, int64(3), page2.Items[0].VideoID)
	require.Equal(t, int64(2), page2.Items[1].VideoID)
	require.True(t, page2.HasMore)

	page3, err := svc.List(context.Background(), 2, page2.NextCursor)
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

	page2, err := svc.List(context.Background(), 2, page1.NextCursor)
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
	repo := &fakeRepo{videos: fiveVideos()}
	cache := &fakeCache{store: &FeedResult{Items: []FeedItem{
		{VideoID: 99, Title: "cached", VideoURL: "http://cache.local/v99"},
	}}}
	svc := newTestServiceWithCache(repo, cache)

	res, err := svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	require.Equal(t, int64(99), res.Items[0].VideoID)
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
