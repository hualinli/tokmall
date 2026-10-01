package video

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/storage"
)

type fakeRepo struct {
	videos      map[int64]*Video
	findCalls   int
	nextID      int64
	tickets     map[string]*UploadTicket // key: objectKey
	likes       map[string]bool          // key: "{userID}-{videoID}"
	playEvents  map[string]*PlayEvent    // key: canonical eventID（保存完整事实供幂等比较）
	playMu      sync.Mutex
	updateCalls []updateStatusCall
}

type updateStatusCall struct {
	videoID     int64
	authorID    int64
	from        Status
	to          Status
	publishedAt *time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		videos:     make(map[int64]*Video),
		nextID:     1,
		tickets:    make(map[string]*UploadTicket),
		likes:      make(map[string]bool),
		playEvents: make(map[string]*PlayEvent),
	}
}

func (f *fakeRepo) CreateVideo(ctx context.Context, v *Video) error {
	v.ID = f.nextID
	f.nextID++
	f.videos[v.ID] = v
	return nil
}

func (f *fakeRepo) FindVideoByID(ctx context.Context, id int64) (*Video, error) {
	f.findCalls++
	v, ok := f.videos[id]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}

func (f *fakeRepo) UpdateVideoStatus(ctx context.Context, id, authorID int64, from, to Status, publishedAt *time.Time) error {
	f.updateCalls = append(f.updateCalls, updateStatusCall{
		videoID: id, authorID: authorID, from: from, to: to, publishedAt: publishedAt,
	})
	v, ok := f.videos[id]
	if !ok || v.AuthorID != authorID || v.Status != from {
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

// ConsumeTicketAndCreateVideo 复刻事务语义：核销失败则视频不落库。
func (f *fakeRepo) ConsumeTicketAndCreateVideo(ctx context.Context, userID int64, objectKey string, v *Video) error {
	if err := f.ConsumeTicket(ctx, userID, objectKey); err != nil {
		return err
	}
	return f.CreateVideo(ctx, v)
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
		// 比游标更新的行直接跳过（正面表述，避免否定复合条件）。
		if before != nil {
			newerThanCursor := v.CreatedAt.After(before.CreatedAt) ||
				(v.CreatedAt.Equal(before.CreatedAt) && v.ID >= before.VideoID)
			if newerThanCursor {
				continue
			}
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

func (f *fakeRepo) CreatePlayEvent(_ context.Context, e *PlayEvent) (*PlayEventCreateResult, error) {
	f.playMu.Lock()
	defer f.playMu.Unlock()
	if existing, ok := f.playEvents[e.EventID]; ok {
		copy := *existing
		return &PlayEventCreateResult{Existing: &copy}, nil
	}
	copy := *e
	f.playEvents[e.EventID] = &copy
	return &PlayEventCreateResult{Inserted: true}, nil
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
	require.Len(t, repo.updateCalls, 1)
	require.Equal(t, int64(42), repo.updateCalls[0].authorID,
		"发布更新必须把当前用户作为 author_id 条件传入 repository")
	require.Equal(t, StatusPendingReview, repo.updateCalls[0].from)
	require.Equal(t, StatusPublished, repo.updateCalls[0].to)

	// 重复 publish：状态机拒绝
	_, err = svc.Publish(context.Background(), 42, res.VideoID)
	require.ErrorIs(t, err, ErrStatusConflict)
}

func TestPublish_NotFoundAndUnauthorizedHidden(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)

	_, err := svc.Publish(context.Background(), 42, 404)
	require.ErrorIs(t, err, ErrNotFound, "不存在的视频必须返回 not found")

	registerTicket(t, repo, 42, "videos/42/u1.mp4")
	store.objectSizes["videos/42/u1.mp4"] = 1024
	created, err := svc.Create(context.Background(), 42, CreateParams{
		Title: "private", ObjectKey: "videos/42/u1.mp4", DurationSeconds: 10,
	})
	require.NoError(t, err)

	_, err = svc.Publish(context.Background(), 99, created.VideoID)
	require.ErrorIs(t, err, ErrNotFound, "非作者不能通过发布接口探测未发布视频")
	v, err := repo.FindVideoByID(context.Background(), created.VideoID)
	require.NoError(t, err)
	require.Equal(t, StatusPendingReview, v.Status, "越权请求不能改变状态")
	require.Empty(t, repo.updateCalls, "非作者在授权检查失败后不应触发写入")

	v.Status = StatusDeleted
	_, err = svc.Publish(context.Background(), 42, created.VideoID)
	require.ErrorIs(t, err, ErrNotFound, "已删除视频对所有人都不可见")
}

// concurrentPublishRepo 只为并发发布测试提供真实的条件更新语义：
// Find 可以同时读取，Update 使用互斥锁模拟数据库的单条 CAS 更新。
type concurrentPublishRepo struct {
	*fakeRepo
	mu    sync.Mutex
	video Video
}

func (r *concurrentPublishRepo) FindVideoByID(_ context.Context, id int64) (*Video, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.video.ID != id {
		return nil, ErrNotFound
	}
	v := r.video
	return &v, nil
}

func (r *concurrentPublishRepo) UpdateVideoStatus(_ context.Context, id, authorID int64, from, to Status, publishedAt *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.video.ID != id || r.video.AuthorID != authorID || r.video.Status != from {
		return ErrStatusConflict
	}
	r.video.Status = to
	r.video.PublishedAt = publishedAt
	return nil
}

func TestPublish_ConcurrentSingleWinner(t *testing.T) {
	repo := &concurrentPublishRepo{
		fakeRepo: newFakeRepo(),
		video:    Video{ID: 7, AuthorID: 42, Status: StatusPendingReview},
	}
	store := newFakeStorage()
	svc := NewService(repo, store, 15*time.Minute, "test-cursor-secret", nil)

	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Publish(context.Background(), 42, repo.video.ID)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var success, conflicts int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrStatusConflict):
			conflicts++
		default:
			t.Fatalf("unexpected publish error: %v", err)
		}
	}
	require.Equal(t, 1, success, "同一视频并发发布只能有一个成功者")
	require.Equal(t, 1, conflicts, "竞争失败者必须收到状态冲突")
	repo.mu.Lock()
	require.Equal(t, StatusPublished, repo.video.Status)
	repo.mu.Unlock()
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

	// 非作者与不存在统一 404：403 会成为"这个视频 ID 存在"的预言机。
	err = svc.Delete(context.Background(), 99, res.VideoID)
	require.ErrorIs(t, err, ErrNotFound)

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
	require.ErrorIs(t, svc.Delete(context.Background(), 99, res.VideoID), ErrNotFound)
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

	page2, err := svc.ListMine(context.Background(), 42, 2, cursorOrEmpty(page1.NextCursor))
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
	eventID := uuid.NewString()
	err := svc.Play(context.Background(), 7, videoID, eventID, nil, &playedAt, nil)
	require.NoError(t, err)
	require.NotNil(t, repo.playEvents[eventID], "事件应已落库")
}

func TestPlay_IdempotentDuplicate(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/u1.mp4")

	eventID := uuid.NewString()
	err := svc.Play(context.Background(), 7, videoID, eventID, nil, nil, nil)
	require.NoError(t, err)
	err = svc.Play(context.Background(), 7, videoID, eventID, nil, nil, nil)
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
	err = svc.Play(context.Background(), 7, res.VideoID, uuid.NewString(), nil, nil, nil)
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, repo.playEvents)

	// 已删除：同样拒绝
	_, err = svc.Publish(context.Background(), 42, res.VideoID)
	require.NoError(t, err)
	require.NoError(t, svc.Delete(context.Background(), 42, res.VideoID))
	err = svc.Play(context.Background(), 7, res.VideoID, uuid.NewString(), nil, nil, nil)
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

	for _, invalid := range []string{
		"not-a-uuid",
		"0191b3e1-5f0a-7f2b-9c3d-4e5f6a7b8c9",           // 少一位
		"urn:uuid:0191b3e1-5f0a-7f2b-9c3d-4e5f6a7b8c9d", // 非 API canonical 文本
		"00000000-0000-0000-0000-000000000000",          // nil UUID
	} {
		err = svc.Play(context.Background(), 7, videoID, invalid, nil, nil, nil)
		require.ErrorIs(t, err, ErrInvalidParam, "非法 event_id 必须在 service 边界拒绝: %q", invalid)
	}

	future := time.Now().Add(1 * time.Hour)
	err = svc.Play(context.Background(), 7, videoID, uuid.NewString(), nil, &future, nil)
	require.ErrorIs(t, err, ErrInvalidParam, "未来事件时间拒绝")
}

func TestPlay_RespectsClientPlayedAt(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/u1.mp4")

	playedAt := time.Now().Add(-2 * time.Hour)
	session := "sess-1"
	err := svc.Play(context.Background(), 7, videoID, uuid.NewString(), &session, &playedAt, nil)
	require.NoError(t, err)
	require.Len(t, repo.playEvents, 1)
}

func TestPlay_AcceptsUUIDCaseInsensitivelyAndCanonicalizes(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/uuid-case.mp4")

	raw := strings.ToUpper(uuid.NewString())
	err := svc.Play(context.Background(), 7, videoID, raw, nil, nil, nil)
	require.NoError(t, err)
	require.Contains(t, repo.playEvents, strings.ToLower(raw), "UUID 文本应统一为数据库 canonical 形式")
	require.NotContains(t, repo.playEvents, raw)
}

func TestPlay_IdempotencyConflictOnDifferentBusinessFact(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/idempotency-1.mp4")
	otherVideoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/idempotency-2.mp4")

	eventID := uuid.NewString()
	playedAt := time.Date(2026, 9, 16, 6, 0, 0, 123456789, time.UTC)
	session := "session-a"
	err := svc.Play(context.Background(), 7, videoID, eventID, &session, &playedAt, nil)
	require.NoError(t, err)

	cases := []struct {
		name    string
		userID  int64
		videoID int64
		session string
		at      time.Time
	}{
		{name: "different user", userID: 8, videoID: videoID, session: session, at: playedAt},
		{name: "different video", userID: 7, videoID: otherVideoID, session: session, at: playedAt},
		{name: "different session", userID: 7, videoID: videoID, session: "session-b", at: playedAt},
		{name: "different played_at", userID: 7, videoID: videoID, session: session, at: playedAt.Add(time.Microsecond)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Play(context.Background(), tc.userID, tc.videoID, eventID,
				&tc.session, &tc.at, nil)
			require.ErrorIs(t, err, ErrIdempotencyConflict,
				"同一 event_id 复用到不同业务事实必须显式冲突")
		})
	}

	// trace_id 是请求级观测元数据，重试时允许变化，不应把同一业务事实误判为冲突。
	trace := "different-request-trace"
	err = svc.Play(context.Background(), 7, videoID, eventID, &session, &playedAt, &trace)
	require.NoError(t, err)
}

func TestPlay_IdempotencyNormalizesPostgresTimestampPrecision(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/idempotency-time.mp4")

	eventID := uuid.NewString()
	// Go time 可携带纳秒，但 PostgreSQL TIMESTAMPTZ 只保存微秒；两次相同
	// 请求都必须在 service 入口归一化后再做事实比较。
	playedAt := time.Date(2026, 9, 16, 6, 0, 0, 123456789, time.UTC)
	err := svc.Play(context.Background(), 7, videoID, eventID, nil, &playedAt, nil)
	require.NoError(t, err)
	err = svc.Play(context.Background(), 7, videoID, eventID, nil, &playedAt, nil)
	require.NoError(t, err)
	require.Len(t, repo.playEvents, 1)
	require.Equal(t, 123456*time.Microsecond, repo.playEvents[strings.ToLower(eventID)].PlayedAt.Sub(
		time.Date(2026, 9, 16, 6, 0, 0, 0, time.UTC)),
		"落库事实应按微秒精度比较")
}

func TestPlay_ConcurrentSameEventIDIsIdempotent(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	videoID := publishTestVideo(t, svc, repo, store, 42, "videos/42/idempotency-concurrent.mp4")

	const callers = 32
	eventID := uuid.NewString()
	playedAt := time.Date(2026, 9, 16, 6, 0, 0, 123456789, time.UTC)
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- svc.Play(context.Background(), 7, videoID, eventID, nil, &playedAt, nil)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err, "相同 event_id 的并发重试都应收敛为幂等成功")
	}
	require.Len(t, repo.playEvents, 1, "唯一约束语义下并发重试只能落一条事实")
}

// cursorOrEmpty 解引用可空游标（nil → ""，适配 *string 的 NextCursor）。
func cursorOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestPlayEventInputNormalization：session_id / trace_id 必须在入口归一。
// 超长值会被 PostgreSQL 以 22001 拒绝（列宽 128/64），最终表现为 500；
// trace_id 还只接受服务端生成的 UUID，避免观测字段被客户端伪造。
func TestPlayEventInputNormalization(t *testing.T) {
	long := strings.Repeat("s", maxSessionIDLength+1)
	require.Nil(t, normalizePlayEventSessionID(&long), "超长 session_id 必须丢弃而不是透传")
	blank := "   "
	require.Nil(t, normalizePlayEventSessionID(&blank))
	ok := "session-1"
	require.Equal(t, &ok, normalizePlayEventSessionID(&ok))

	overlongTrace := strings.Repeat("t", maxTraceIDLength+1)
	require.Nil(t, normalizeTraceID(&overlongTrace))
	forged := "not-a-uuid"
	require.Nil(t, normalizeTraceID(&forged), "客户端伪造的 trace_id 不得入库")
	valid := "0192f0aa-1111-7abc-8def-0123456789ab"
	require.Equal(t, &valid, normalizeTraceID(&valid))
}
