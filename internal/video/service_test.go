package video

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lihualin/tokmall/internal/storage"
	"github.com/stretchr/testify/require"
)

type fakeRepo struct {
	videos     map[int64]*Video
	nextID     int64
	tickets    map[string]*UploadTicket // key: objectKey
	likes      map[string]bool          // key: "{userID}-{videoID}"
	playEvents map[string]bool          // key: eventID（幂等去重）
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		videos:     make(map[int64]*Video),
		nextID:     1,
		tickets:    make(map[string]*UploadTicket),
		likes:      make(map[string]bool),
		playEvents: make(map[string]bool),
	}
}

func (f *fakeRepo) CreateVideo(ctx context.Context, v *Video) error {
	v.ID = f.nextID
	f.nextID++
	f.videos[v.ID] = v
	return nil
}

func (f *fakeRepo) FindVideoByID(ctx context.Context, id int64) (*Video, error) {
	v, ok := f.videos[id]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}

func (f *fakeRepo) UpdateVideoStatus(ctx context.Context, id int64, from, to Status, publishedAt *time.Time) error {
	v, ok := f.videos[id]
	if !ok || v.Status != from {
		return ErrStatusConflict
	}
	v.Status = to
	v.PublishedAt = publishedAt
	return nil
}

func (f *fakeRepo) CreateTicket(ctx context.Context, t *UploadTicket) error {
	f.tickets[t.ObjectKey] = t
	return nil
}

func (f *fakeRepo) ConsumeTicket(ctx context.Context, userID int64, objectKey string) error {
	t, ok := f.tickets[objectKey]
	if !ok || t.Used || t.UserID != userID || t.ExpiresAt.Before(time.Now()) {
		return ErrTicketInvalid
	}
	t.Used = true
	return nil
}

func (f *fakeRepo) ListByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error) {
	rows := make([]Video, 0, limit+1)
	for _, v := range f.videos {
		if v.AuthorID == authorID && v.Status != StatusDeleted {
			rows = append(rows, *v)
		}
	}
	// 与生产 SQL 一致：(created_at, id) 倒序
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].ID > rows[j].ID
	})
	out := make([]Video, 0, limit+1)
	for _, v := range rows {
		if before != nil && !(v.CreatedAt.Before(before.CreatedAt) ||
			(v.CreatedAt.Equal(before.CreatedAt) && v.ID < before.VideoID)) {
			continue
		}
		out = append(out, v)
		if len(out) >= limit+1 {
			break
		}
	}
	return out, nil
}

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

func (f *fakeRepo) IsLiked(_ context.Context, userID, videoID int64) (bool, error) {
	return f.likes[fmt.Sprintf("%d-%d", userID, videoID)], nil
}

func (f *fakeRepo) CreatePlayEvent(_ context.Context, e *PlayEvent) error {
	if f.playEvents[e.EventID] {
		return nil
	}
	f.playEvents[e.EventID] = true
	return nil
}

type fakeStorage struct {
	objectSizes map[string]int64
	deletedKeys []string
	presignErr  error
	statErr     error
	deleteErr   error
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objectSizes: make(map[string]int64)}
}

func (f *fakeStorage) PresignPutURL(ctx context.Context, objectKey string) (string, error) {
	if f.presignErr != nil {
		return "", f.presignErr
	}
	return "http://minio.local/put/" + objectKey, nil
}

func (f *fakeStorage) StatObject(ctx context.Context, objectKey string) (int64, error) {
	if f.statErr != nil {
		return 0, f.statErr
	}
	size, ok := f.objectSizes[objectKey]
	if !ok {
		return 0, storage.ErrObjectNotFound
	}
	return size, nil
}

func (f *fakeStorage) PresignGetURL(ctx context.Context, objectKey string) (string, error) {
	return "http://minio.local/signed/" + objectKey, nil
}

func (f *fakeStorage) PresignAvatarURL(ctx context.Context, objectKey string) (string, error) {
	return "http://minio.local/avatar/" + objectKey, nil
}

func (f *fakeStorage) DeleteObject(ctx context.Context, objectKey string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedKeys = append(f.deletedKeys, objectKey)
	delete(f.objectSizes, objectKey)
	return nil
}

func newTestService(repo *fakeRepo, store *fakeStorage) *Service {
	return NewService(repo, store, 15*time.Minute, "test-cursor-secret", newFakeInvalidator())
}

// fakeInvalidator 记录失效调用：验证"发布/删除成功才失效"与"失效失败不阻塞请求"。
type fakeInvalidator struct {
	calls int
	err   error
}

func newFakeInvalidator() *fakeInvalidator {
	return &fakeInvalidator{}
}

func (f *fakeInvalidator) InvalidateFeedCache(ctx context.Context) error {
	f.calls++
	return f.err
}

func registerTicket(t *testing.T, repo *fakeRepo, userID int64, objectKey string) {
	t.Helper()
	err := repo.CreateTicket(context.Background(), &UploadTicket{
		UserID:    userID,
		ObjectKey: objectKey,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	require.NoError(t, err)
}

func TestGetUploadURL_Valid(t *testing.T) {
	svc := newTestService(newFakeRepo(), newFakeStorage())

	res, err := svc.GetUploadURL(context.Background(), 42, "clip.mp4", 10<<20, "video/mp4")
	require.NoError(t, err)
	require.Contains(t, res.ObjectKey, "videos/42/")
	require.Contains(t, res.UploadURL, "http://minio.local/put/")
	require.False(t, res.ExpiresAt.Before(time.Now()))
}

func TestGetUploadURL_Validation(t *testing.T) {
	svc := newTestService(newFakeRepo(), newFakeStorage())

	cases := []struct {
		name        string
		fileSize    int64
		contentType string
	}{
		{"too small", 100, "video/mp4"},
		{"too large", 513 << 20, "video/mp4"},
		{"bad type", 10 << 20, "image/png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.GetUploadURL(context.Background(), 42, "clip.mp4", tc.fileSize, tc.contentType)
			require.ErrorIs(t, err, ErrInvalidParam)
		})
	}
}

func TestCreate_Success(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024

	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title:           "test video",
		ObjectKey:       "videos/42/u1.mp4",
		DurationSeconds: 45,
	})
	require.NoError(t, err)
	require.Equal(t, StatusPendingReview, res.Status)

	v, err := repo.FindVideoByID(context.Background(), res.VideoID)
	require.NoError(t, err)
	require.Equal(t, int64(42), v.AuthorID)
	require.Equal(t, StatusPendingReview, v.Status)
	// ticket 已核销，不能复用
	_, err = svc.Create(context.Background(), 42, CreateParams{
		Title: "dup", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.ErrorIs(t, err, ErrUploadURLExpired)
}

func TestCreate_InvalidParams(t *testing.T) {
	svc := newTestService(newFakeRepo(), newFakeStorage())

	_, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "", ObjectKey: "k", DurationSeconds: 10,
	})
	require.ErrorIs(t, err, ErrInvalidParam)

	_, err = svc.Create(context.Background(), 42, CreateParams{
		Title: "ok", ObjectKey: "k", DurationSeconds: 99999,
	})
	require.ErrorIs(t, err, ErrInvalidParam)
}

func TestCreate_ForgedObjectKey(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	store.objectSizes["forged.mp4"] = 1024

	_, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "forged", ObjectKey: "forged.mp4", DurationSeconds: 10,
	})
	require.ErrorIs(t, err, ErrUploadURLExpired, "没有 ticket 的 key 必须被拒绝")
}

func TestCreate_ObjectMissingOnMinIO(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	// 未在 store.objectSizes 登记 = MinIO 上不存在

	_, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "empty", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.ErrorIs(t, err, ErrUploadURLExpired, "对象不存在是坏输入（400），不是系统故障（500）")
}

func TestPublish_SuccessAndConflict(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	pub, err := svc.Publish(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	require.Equal(t, StatusPublished, pub.Status)
	require.NotNil(t, pub.Status)

	// 重复 publish：状态机拒绝
	_, err = svc.Publish(context.Background(), 42, res.VideoID)
	require.ErrorIs(t, err, ErrStatusConflict)
}

func TestGet_VisibilityPolicy(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	// 作者自己能看到未发布视频
	own, err := svc.Get(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	require.Equal(t, StatusPendingReview, own.Status)

	// 他人看未发布视频：404，不泄露存在性
	_, err = svc.Get(context.Background(), 99, res.VideoID)
	require.ErrorIs(t, err, ErrNotFound)

	// 发布后所有人可见
	_, err = svc.Publish(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	other, err := svc.Get(context.Background(), 99, res.VideoID)
	require.NoError(t, err)
	require.Equal(t, StatusPublished, other.Status)
}

func TestDelete_ForbiddenAndSuccess(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	err = svc.Delete(context.Background(), 99, res.VideoID)
	require.ErrorIs(t, err, ErrForbidden)

	err = svc.Delete(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	v, err := repo.FindVideoByID(context.Background(), res.VideoID)
	require.NoError(t, err)
	require.Equal(t, StatusDeleted, v.Status)

	// 已删除对任何人（含作者）都是 404
	_, err = svc.Get(context.Background(), 42, res.VideoID)
	require.ErrorIs(t, err, ErrNotFound)

	// 已删除是终态，不能再删
	err = svc.Delete(context.Background(), 42, res.VideoID)
	require.ErrorIs(t, err, ErrStatusConflict)
}

// TestPublishInvalidatesCache 发布成功必须失效 feed 缓存。
func TestPublishInvalidatesCache(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	invalidator := newFakeInvalidator()
	svc := NewService(repo, store, 15*time.Minute, "test-cursor-secret", invalidator)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	_, err = svc.Publish(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	require.Equal(t, 1, invalidator.calls, "发布成功必须失效缓存")

	// 状态冲突的发布（未成功）不能失效
	_, err = svc.Publish(context.Background(), 42, res.VideoID)
	require.ErrorIs(t, err, ErrStatusConflict)
	require.Equal(t, 1, invalidator.calls, "失败的发布不得触发失效")
}

// TestDeleteInvalidatesCache 删除成功必须失效 feed 缓存。
func TestDeleteInvalidatesCache(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	invalidator := newFakeInvalidator()
	svc := NewService(repo, store, 15*time.Minute, "test-cursor-secret", invalidator)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), 42, res.VideoID))
	require.Equal(t, 1, invalidator.calls, "删除成功必须失效缓存")

	// 越权删除（未成功）不能失效
	require.ErrorIs(t, svc.Delete(context.Background(), 99, res.VideoID), ErrForbidden)
	require.Equal(t, 1, invalidator.calls, "失败的删除不得触发失效")
}

// TestInvalidateFailureDoesNotFailRequest 缓存失效失败不能阻塞业务：
// DB 已写入，脏窗口由 TTL 兜底。
func TestInvalidateFailureDoesNotFailRequest(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	invalidator := newFakeInvalidator()
	invalidator.err = errors.New("redis down")
	svc := NewService(repo, store, 15*time.Minute, "test-cursor-secret", invalidator)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	pub, err := svc.Publish(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	require.Equal(t, StatusPublished, pub.Status)
	require.Equal(t, 1, invalidator.calls)
}

// seedVideosFor 给指定作者造 n 条视频（created_at 递增，id 递增）。
func seedVideosFor(t *testing.T, repo *fakeRepo, authorID int64, n int) []int64 {
	t.Helper()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		repo.CreateVideo(context.Background(), &Video{
			AuthorID:  authorID,
			Title:     fmt.Sprintf("u%d-v%d", authorID, i+1),
			ObjectKey: fmt.Sprintf("videos/%d/seed%d.mp4", authorID, i+1),
			Status:    StatusPublished,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
		ids = append(ids, repo.nextID-1)
	}
	return ids
}

// TestListMine_Pagination 作者列表：created_at 倒序、键集翻页无重叠、DELETED 排除。
func TestListMine_Pagination(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo, newFakeStorage())
	ids := seedVideosFor(t, repo, 42, 5)
	// 删掉 id 最小的那条（最早创建）→ 列表里不应出现
	repo.videos[ids[0]].Status = StatusDeleted

	page1, err := svc.ListMine(context.Background(), 42, 2, "")
	require.NoError(t, err)
	require.Len(t, page1.Items, 2)
	require.Equal(t, ids[4], page1.Items[0].VideoID, "最新创建的排最前")
	require.Equal(t, ids[3], page1.Items[1].VideoID)
	require.True(t, page1.HasMore)

	page2, err := svc.ListMine(context.Background(), 42, 2, page1.NextCursor)
	require.NoError(t, err)
	require.Len(t, page2.Items, 2)
	require.Equal(t, ids[2], page2.Items[0].VideoID)
	require.Equal(t, ids[1], page2.Items[1].VideoID)
	require.False(t, page2.HasMore, "3 条可用数据已取完")

	seen := map[int64]bool{}
	for _, p := range []*VideoListResult{page1, page2} {
		for _, it := range p.Items {
			require.False(t, seen[it.VideoID], "video %d 重复出现", it.VideoID)
			seen[it.VideoID] = true
		}
	}
	require.Len(t, seen, 4)
	require.False(t, seen[ids[0]], "已删除的不得出现在列表")
}

func TestListMine_OtherAuthorsHidden(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo, newFakeStorage())
	seedVideosFor(t, repo, 42, 2)
	seedVideosFor(t, repo, 7, 3)

	res, err := svc.ListMine(context.Background(), 42, 20, "")
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	for _, it := range res.Items {
		require.Contains(t, it.Title, "u42-", "列表里不得出现其他作者的视频")
	}
}

func TestListMine_InvalidInputs(t *testing.T) {
	svc := newTestService(newFakeRepo(), newFakeStorage())

	_, err := svc.ListMine(context.Background(), 42, 101, "")
	require.ErrorIs(t, err, ErrInvalidParam)

	_, err = svc.ListMine(context.Background(), 42, -1, "")
	require.ErrorIs(t, err, ErrInvalidParam)

	_, err = svc.ListMine(context.Background(), 42, 20, "forged.cursor")
	require.ErrorIs(t, err, ErrInvalidCursor)
}

// TestDeleteRemovesObjects 删除必须连带删 MinIO 对象（软删 ≠ 内容不可达）。
func TestDeleteRemovesObjects(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	cover := "covers/42/c1.jpg"
	registerTicket(t, repo, 42, cover)
	store.objectSizes[cover] = 2048
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", CoverKey: &cover, DurationSeconds: 10,
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), 42, res.VideoID))
	require.ElementsMatch(t, []string{"videos/42/u1.mp4", "covers/42/c1.jpg"}, store.deletedKeys)
}

// TestDeleteObjectFailureStillSucceeds 对象删除失败不能阻塞删除业务：
// status 已 DELETED，孤儿对象由清理任务收敛。
func TestDeleteObjectFailureStillSucceeds(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	store.deleteErr = errors.New("minio down")
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), 42, res.VideoID))

	v, err := repo.FindVideoByID(context.Background(), res.VideoID)
	require.NoError(t, err)
	require.Equal(t, StatusDeleted, v.Status)
}

// TestCursor_RoundTrip 作者列表游标：签名防篡改 + 版本校验。
func TestCursor_RoundTrip(t *testing.T) {
	codec := NewCodec("test-cursor-secret")
	cur := Cursor{CreatedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), VideoID: 7, RankVersion: CurrentRankVersion}

	enc, err := codec.Encode(cur)
	require.NoError(t, err)
	dec, err := codec.Decode(enc)
	require.NoError(t, err)
	require.Equal(t, cur, *dec)

	_, err = codec.Decode("forged.cursor")
	require.ErrorIs(t, err, ErrInvalidCursor)

	tampered := enc[:len(enc)-1]
	if enc[len(enc)-1] == 'a' {
		tampered += "b"
	} else {
		tampered += "a"
	}
	_, err = codec.Decode(tampered)
	require.ErrorIs(t, err, ErrInvalidCursor)

	old := cur
	old.RankVersion = "author-v0"
	enc, err = codec.Encode(old)
	require.NoError(t, err)
	_, err = codec.Decode(enc)
	require.ErrorIs(t, err, ErrInvalidCursor)
}

// publishTestVideo 走完整 Create+Publish 流程，返回一个 PUBLISHED 视频。
func publishTestVideo(t *testing.T, svc *Service, repo *fakeRepo, store *fakeStorage, userID int64, objectKey string) int64 {
	t.Helper()
	registerTicket(t, repo, userID, objectKey)
	store.objectSizes[objectKey] = 1024
	res, err := svc.Create(context.Background(), userID, CreateParams{
		Title: "t", ObjectKey: objectKey, DurationSeconds: 10,
	})
	require.NoError(t, err)
	_, err = svc.Publish(context.Background(), userID, res.VideoID)
	require.NoError(t, err)
	return res.VideoID
}

func TestPlay_RecordsEvent(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/u1.mp4")

	playedAt := time.Now().Add(-30 * time.Second)
	err := svc.Play(context.Background(), 7, videoID, "event-1", nil, &playedAt, nil)
	require.NoError(t, err)
	require.True(t, repo.playEvents["event-1"], "事件应已落库")
}

func TestPlay_IdempotentDuplicate(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/u1.mp4")

	err := svc.Play(context.Background(), 7, videoID, "same-event", nil, nil, nil)
	require.NoError(t, err)
	err = svc.Play(context.Background(), 7, videoID, "same-event", nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, len(repo.playEvents), "同一 event_id 重放只记一次")
}

func TestPlay_NotPublishedRejected(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	res, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "t", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)
	// 未发布（PENDING_REVIEW）：不接受播放事件
	err = svc.Play(context.Background(), 7, res.VideoID, "event-pending", nil, nil, nil)
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, repo.playEvents)

	// 已删除：同样拒绝
	_, err = svc.Publish(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	require.NoError(t, svc.Delete(context.Background(), 42, res.VideoID))
	err = svc.Play(context.Background(), 7, res.VideoID, "event-deleted", nil, nil, nil)
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, repo.playEvents)
}

func TestPlay_InvalidInputs(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/u1.mp4")

	err := svc.Play(context.Background(), 7, videoID, "", nil, nil, nil)
	require.ErrorIs(t, err, ErrInvalidParam, "空 event_id 拒绝")

	future := time.Now().Add(1 * time.Hour)
	err = svc.Play(context.Background(), 7, videoID, "event-future", nil, &future, nil)
	require.ErrorIs(t, err, ErrInvalidParam, "未来事件时间拒绝")
}

func TestPlay_RespectsClientPlayedAt(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/u1.mp4")

	playedAt := time.Now().Add(-2 * time.Hour)
	session := "sess-1"
	err := svc.Play(context.Background(), 7, videoID, "event-2", &session, &playedAt, nil)
	require.NoError(t, err)
	require.True(t, repo.playEvents["event-2"])
}
