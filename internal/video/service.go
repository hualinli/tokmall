package video

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/lihualin/tokmall/internal/storage"
)

var (
	ErrInvalidParam     = errors.New("invalid param")
	ErrForbidden        = errors.New("forbidden")
	ErrUploadURLExpired = errors.New("upload url expired or never issued")
)

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// Repository 由 service 定义、消费，GormRepository 隐式满足。
type Repository interface {
	CreateVideo(ctx context.Context, v *Video) error
	FindVideoByID(ctx context.Context, id int64) (*Video, error)
	UpdateVideoStatus(ctx context.Context, id int64, from, to Status, publishedAt *time.Time) error
	CreateTicket(ctx context.Context, t *UploadTicket) error
	ConsumeTicket(ctx context.Context, userID int64, objectKey string) error
	ListByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error)
}

// Storage 同样由 service 定义：最小化依赖面，方便单测注入 fake。
type Storage interface {
	PresignPutURL(ctx context.Context, objectKey string) (string, error)
	PresignGetURL(ctx context.Context, objectKey string) (string, error)
	StatObject(ctx context.Context, objectKey string) (int64, error)
	DeleteObject(ctx context.Context, objectKey string) error
}

// FeedCacheInvalidator 消费侧接口：发布/删除成功后失效 feed 首页缓存。
// 实现是 feed.RedisFeedCache（main 装配），video 不知道 feed 存在。
type FeedCacheInvalidator interface {
	InvalidateFeedCache(ctx context.Context) error
}

type Service struct {
	repo      Repository
	storage   Storage
	expiry    time.Duration
	feedCache FeedCacheInvalidator
	cursor    *codec
}

func NewService(repo Repository, storage Storage, uploadExpiry time.Duration, cursorSecret string, feedCache FeedCacheInvalidator) *Service {
	return &Service{
		repo:      repo,
		storage:   storage,
		expiry:    uploadExpiry,
		feedCache: feedCache,
		cursor:    NewCodec(cursorSecret),
	}
}

// invalidateFeed 先写库、后删缓存（Cache-Aside 标准动作）。
// 失效失败只记 Warn 不回滚请求：DB 已写入，脏窗口由 TTL 兜底
// （最坏 60s 后自动恢复）。缓存失效绝不能进入请求的成功路径。
func (s *Service) invalidateFeed(ctx context.Context) {
	if s.feedCache == nil {
		return
	}
	if err := s.feedCache.InvalidateFeedCache(ctx); err != nil {
		slog.Warn("invalidate feed cache failed", "err", err)
	}
}

// statusTransitions 状态机：key 是当前状态，value 是允许迁移到的状态集合。
var statusTransitions = map[Status][]Status{
	StatusUploading:     {StatusProcessing, StatusFailed},
	StatusProcessing:    {StatusPendingReview, StatusFailed},
	StatusPendingReview: {StatusPublished, StatusBlocked, StatusFailed, StatusDeleted},
	StatusPublished:     {StatusBlocked, StatusDeleted},
	StatusBlocked:       {StatusDeleted},
	StatusFailed:        {StatusDeleted},
}

func canTransition(from, to Status) bool {
	for _, s := range statusTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

type UploadResult struct {
	ObjectKey string    `json:"object_key"`
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CreateParams struct {
	Title           string
	ObjectKey       string
	CoverKey        *string
	DurationSeconds int
}

type CreateResult struct {
	VideoID int64  `json:"video_id"`
	Status  Status `json:"status"`
}

type PublishResult struct {
	VideoID int64  `json:"video_id"`
	Status  Status `json:"status"`
}

type VideoDetail struct {
	VideoID         int64      `json:"video_id"`
	AuthorID        int64      `json:"author_id"`
	Title           string     `json:"title"`
	VideoURL        string     `json:"video_url"`
	DurationSeconds int        `json:"duration_seconds"`
	Status          Status     `json:"status"`
	PublishedAt     *time.Time `json:"published_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

type VideoListItem struct {
	VideoID         int64      `json:"video_id"`
	Title           string     `json:"title"`
	VideoURL        string     `json:"video_url"`
	CoverURL        *string    `json:"cover_url"`
	DurationSeconds int        `json:"duration_seconds"`
	Status          Status     `json:"status"`
	PublishedAt     *time.Time `json:"published_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

type VideoListResult struct {
	Items      []VideoListItem `json:"items"`
	NextCursor string          `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
}

// GetUploadURL 发放预签名上传 URL 并记录 ticket。
func (s *Service) GetUploadURL(ctx context.Context, userID int64, filename string, fileSize int64, contentType string) (*UploadResult, error) {
	if fileSize < 1<<20 || fileSize > 512<<20 {
		return nil, ErrInvalidParam
	}
	switch contentType {
	case "video/mp4", "video/webm", "video/quicktime":
	default:
		return nil, ErrInvalidParam
	}
	objectKey := fmt.Sprintf("videos/%d/%s.mp4", userID, uuid.NewString())
	uploadURL, err := s.storage.PresignPutURL(ctx, objectKey)
	if err != nil {
		slog.Error("presign upload url failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("presign upload url: %w", err)
	}
	err = s.repo.CreateTicket(ctx, &UploadTicket{
		UserID:    userID,
		ObjectKey: objectKey,
		ExpiresAt: time.Now().Add(s.expiry),
	})
	if err != nil {
		slog.Error("create ticket failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("create ticket: %w", err)
	}
	return &UploadResult{
		ObjectKey: objectKey,
		UploadURL: uploadURL,
		ExpiresAt: time.Now().Add(s.expiry),
	}, nil
}

// Create 校验 ticket 并确认文件真实上传后创建视频记录（PENDING_REVIEW）。
func (s *Service) Create(ctx context.Context, userID int64, p CreateParams) (*CreateResult, error) {
	title := p.Title
	if title == "" || len(title) > 200 {
		return nil, ErrInvalidParam
	}
	if p.DurationSeconds < 1 || p.DurationSeconds > 3600 {
		return nil, ErrInvalidParam
	}
	size, err := s.storage.StatObject(ctx, p.ObjectKey)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return nil, ErrUploadURLExpired
		}
		slog.Error("stat object failed", "object_key", p.ObjectKey, "err", err)
		return nil, fmt.Errorf("stat object: %w", err)
	}
	if size == 0 {
		return nil, ErrUploadURLExpired
	}
	err = s.repo.ConsumeTicket(ctx, userID, p.ObjectKey)
	if err != nil {
		if errors.Is(err, ErrTicketInvalid) {
			return nil, ErrUploadURLExpired
		}
		slog.Error("consume ticket failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("consume ticket: %w", err)
	}
	v := &Video{
		AuthorID:        userID,
		Title:           title,
		ObjectKey:       p.ObjectKey,
		CoverKey:        p.CoverKey,
		DurationSeconds: p.DurationSeconds,
		Status:          StatusPendingReview,
	}
	if err := s.repo.CreateVideo(ctx, v); err != nil {
		slog.Error("create video failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("create video: %w", err)
	}
	return &CreateResult{VideoID: v.ID, Status: v.Status}, nil
}

// Publish 作者发布：PENDING_REVIEW -> PUBLISHED。
func (s *Service) Publish(ctx context.Context, userID, videoID int64) (*PublishResult, error) {
	now := time.Now()
	err := s.repo.UpdateVideoStatus(ctx, videoID, StatusPendingReview, StatusPublished, &now)
	if err != nil {
		if errors.Is(err, ErrStatusConflict) {
			return nil, ErrStatusConflict
		}
		slog.Error("publish video failed", "video_id", videoID, "err", err)
		return nil, fmt.Errorf("publish video: %w", err)
	}
	s.invalidateFeed(ctx)
	return &PublishResult{VideoID: videoID, Status: StatusPublished}, nil
}

// Delete 作者删除：任意非终态 -> DELETED。
func (s *Service) Delete(ctx context.Context, userID, videoID int64) error {
	v, err := s.repo.FindVideoByID(ctx, videoID)
	if err != nil {
		return err
	}
	if v.AuthorID != userID {
		return ErrForbidden
	}
	if !canTransition(v.Status, StatusDeleted) {
		return ErrStatusConflict
	}
	err = s.repo.UpdateVideoStatus(ctx, videoID, v.Status, StatusDeleted, nil)
	if err != nil {
		if errors.Is(err, ErrStatusConflict) {
			return ErrStatusConflict
		}
		slog.Error("delete video failed", "video_id", videoID, "err", err)
		return fmt.Errorf("delete video: %w", err)
	}
	s.invalidateFeed(ctx)
	// 软删成功后再删 MinIO 对象：旧 URL 立刻 404，"软删 ≠ 内容不可达"
	// 的缺口在此闭合。对象删除失败只记日志——status 已 DELETED，
	// feed/详情已不可见，孤儿对象由后续清理任务收敛。
	s.removeObjects(ctx, v)
	return nil
}

// removeObjects 删除视频与封面对象（幂等：RemoveObject 对不存在对象不报错）。
func (s *Service) removeObjects(ctx context.Context, v *Video) {
	keys := []string{v.ObjectKey}
	if v.CoverKey != nil {
		keys = append(keys, *v.CoverKey)
	}
	for _, k := range keys {
		if err := s.storage.DeleteObject(ctx, k); err != nil {
			slog.Error("delete object failed", "object_key", k, "err", err)
		}
	}
}

// ListMine 作者的管理视图：自己的全部视频（不含 DELETED），按创建时间倒序。
// 不缓存：QPS 低（只有作者自己看）+ 新鲜性要求高（管理操作后必须立即可见）。
func (s *Service) ListMine(ctx context.Context, userID int64, limit int, cursorRaw string) (*VideoListResult, error) {
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maxListLimit {
		return nil, ErrInvalidParam
	}
	var before *Cursor
	if cursorRaw != "" {
		cur, err := s.cursor.Decode(cursorRaw)
		if err != nil {
			return nil, ErrInvalidCursor
		}
		before = cur
	}
	videos, err := s.repo.ListByAuthor(ctx, userID, before, limit)
	if err != nil {
		slog.Error("list videos by author failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("list by author: %w", err)
	}
	result := &VideoListResult{Items: make([]VideoListItem, 0, limit)}
	hasMore := len(videos) > limit
	if hasMore {
		videos = videos[:limit]
	}
	for _, v := range videos {
		item := VideoListItem{
			VideoID:         v.ID,
			Title:           v.Title,
			DurationSeconds: v.DurationSeconds,
			Status:          v.Status,
			PublishedAt:     v.PublishedAt,
			CreatedAt:       v.CreatedAt,
		}
		if v.CoverKey != nil {
			cover, err := s.storage.PresignGetURL(ctx, *v.CoverKey)
			if err != nil {
				slog.Error("presign cover url failed", "err", err)
				return nil, fmt.Errorf("presign cover url: %w", err)
			}
			item.CoverURL = &cover
		}
		videoURL, err := s.storage.PresignGetURL(ctx, v.ObjectKey)
		if err != nil {
			slog.Error("presign video url failed", "err", err)
			return nil, fmt.Errorf("presign video url: %w", err)
		}
		item.VideoURL = videoURL
		result.Items = append(result.Items, item)
	}
	if hasMore {
		last := videos[len(videos)-1]
		next, err := s.cursor.Encode(Cursor{
			CreatedAt:   last.CreatedAt,
			VideoID:     last.ID,
			RankVersion: CurrentRankVersion,
		})
		if err != nil {
			slog.Error("encode next cursor failed", "err", err)
			return nil, fmt.Errorf("encode next cursor: %w", err)
		}
		result.NextCursor = next
		result.HasMore = true
	}
	return result, nil
}

// Get 按可见性策略返回详情：已删除对所有人不可见；
// 未发布仅作者可见；已发布对所有人可见。
// 不可见一律返回 ErrNotFound（不泄露存在性）。
func (s *Service) Get(ctx context.Context, viewerID, videoID int64) (*VideoDetail, error) {
	v, err := s.repo.FindVideoByID(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if v.Status == StatusDeleted {
		return nil, ErrNotFound
	}
	if v.AuthorID != viewerID && v.Status != StatusPublished {
		return nil, ErrNotFound
	}
	videoURL, err := s.storage.PresignGetURL(ctx, v.ObjectKey)
	if err != nil {
		slog.Error("presign video url failed", "video_id", videoID, "err", err)
		return nil, fmt.Errorf("presign video url: %w", err)
	}
	return &VideoDetail{
		VideoID:         v.ID,
		AuthorID:        v.AuthorID,
		Title:           v.Title,
		VideoURL:        videoURL,
		DurationSeconds: v.DurationSeconds,
		Status:          v.Status,
		PublishedAt:     v.PublishedAt,
		CreatedAt:       v.CreatedAt,
	}, nil
}
