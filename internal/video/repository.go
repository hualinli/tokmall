package video

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

var ErrNotFound = errors.New("record not found")
var ErrStatusConflict = errors.New("status transition not allowed or already applied")
var ErrTicketInvalid = errors.New("upload ticket missing, expired or used")

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

// CreateVideo 显式白名单列：model 上的 Username/AvatarKey 是 JOIN 投影
// （videos 表没有这些列），全字段 Create 会生成错误的 INSERT。
func (r *GormRepository) CreateVideo(ctx context.Context, v *Video) error {
	return r.db.WithContext(ctx).
		Select("author_id", "title", "object_key", "cover_key", "duration_seconds", "status").
		Create(v).Error
}

func (r *GormRepository) FindVideoByID(ctx context.Context, id int64) (*Video, error) {
	var v Video
	err := r.db.WithContext(ctx).Table("videos").
		Select("videos.*, users.username, users.avatar_key").
		Joins("LEFT JOIN users ON users.id = videos.author_id").
		Where("videos.id = ?", id).
		First(&v).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &v, nil
}

// UpdateVideoStatus 带前置状态的条件更新，防止并发重复转换。
// 只有当前状态 == from 时才允许更新到 to，否则返回 ErrStatusConflict。
func (r *GormRepository) UpdateVideoStatus(ctx context.Context, id int64, from, to Status, publishedAt *time.Time) error {
	res := r.db.WithContext(ctx).
		Model(&Video{}).
		Where("id = ? AND status = ?", id, string(from)).
		Updates(map[string]any{
			"status":       string(to),
			"published_at": publishedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStatusConflict
	}
	return nil
}

func (r *GormRepository) CreateTicket(ctx context.Context, t *UploadTicket) error {
	return r.db.WithContext(ctx).Create(t).Error
}

// ListByAuthor 返回作者自己的视频（不含 DELETED），按 (created_at, id) 倒序，
// 以 before 为分界（不含），最多返回 limit+1 条用于判定 has_more。
// 管理视图按创建时间排序：未发布视频没有 published_at，不能用 feed 的键。
// 走 idx_videos_author_created 索引。
func (r *GormRepository) ListByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error) {
	q := r.db.WithContext(ctx).
		Where("author_id = ? AND status <> ?", authorID, string(StatusDeleted))
	if before != nil {
		q = q.Where("(created_at, id) < (?, ?)", before.CreatedAt, before.VideoID)
	}
	var videos []Video
	err := q.
		Order("created_at DESC, id DESC").
		Limit(limit + 1).
		Find(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
}

// LikeCounts 批量查点赞数（跨模块只读 likes 表，详情页用）。
func (r *GormRepository) LikeCounts(ctx context.Context, videoIDs []int64) (map[int64]int64, error) {
	if len(videoIDs) == 0 {
		return map[int64]int64{}, nil
	}
	var rows []struct {
		VideoID int64
		Count   int64
	}
	err := r.db.WithContext(ctx).Table("likes").
		Select("video_id, count(*) AS count").
		Where("video_id IN ?", videoIDs).
		Group("video_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	counts := make(map[int64]int64, len(rows))
	for _, row := range rows {
		counts[row.VideoID] = row.Count
	}
	return counts, nil
}

// IsLiked 判断 viewer 是否已点赞（匿名 viewerID=0 时恒 false）。
func (r *GormRepository) IsLiked(ctx context.Context, userID, videoID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	var count int64
	err := r.db.WithContext(ctx).Table("likes").
		Where("user_id = ? AND video_id = ?", userID, videoID).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ConsumeTicket 原子核销 ticket：未被使用、未过期、属于该用户时标记 used，
// 并返回视频需要的元信息（如上传文件大小由 storage 提供，这里只核销）。
func (r *GormRepository) ConsumeTicket(ctx context.Context, userID int64, objectKey string) error {
	res := r.db.WithContext(ctx).
		Model(&UploadTicket{}).
		Where("user_id = ? AND object_key = ? AND used = false AND expires_at > now()", userID, objectKey).
		Update("used", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTicketInvalid
	}
	return nil
}
