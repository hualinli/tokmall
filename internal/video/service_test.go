package video

import (
	"context"
	"testing"
	"time"

	"github.com/lihualin/tokmall/internal/storage"
	"github.com/stretchr/testify/require"
)

type fakeRepo struct {
	videos  map[int64]*Video
	nextID  int64
	tickets map[string]*UploadTicket // key: objectKey
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		videos:  make(map[int64]*Video),
		nextID:  1,
		tickets: make(map[string]*UploadTicket),
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

type fakeStorage struct {
	objectSizes map[string]int64
	presignErr  error
	statErr     error
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

func (f *fakeStorage) PublicURL(objectKey string) string {
	return "http://minio.local/" + objectKey
}

func newTestService(repo *fakeRepo, store *fakeStorage) *Service {
	return NewService(repo, store, 15*time.Minute)
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
