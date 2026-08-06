package feed

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRepo struct {
	videos []Video // 已按 (published_at, id) 倒序排好，模拟索引顺序
}

func (f *fakeRepo) ListPublished(ctx context.Context, before *Cursor, limit int) ([]Video, error) {
	out := make([]Video, 0, limit+1)
	for _, v := range f.videos {
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
	return out, nil
}

type fakeURLProvider struct{}

func (fakeURLProvider) PublicURL(objectKey string) string {
	return "http://minio.local/" + objectKey
}

func ts(offset time.Duration) *time.Time {
	t := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC).Add(offset)
	return &t
}

func newTestService(repo *fakeRepo) *Service {
	return NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{})
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
	require.Equal(t, "http://minio.local/o5", res.Items[0].VideoURL)
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
