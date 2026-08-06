package feed

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

// ListPublished 返回已发布视频，按 (published_at, id) 倒序，
// 以 before 为分界（不含），最多返回 limit+1 条——多出的 1 条用于判定 has_more。
// before 为 nil 时取第一页。查询走 idx_videos_feed 部分索引。
func (r *GormRepository) ListPublished(ctx context.Context, before *Cursor, limit int) ([]Video, error) {
	q := r.db.WithContext(ctx).
		Where("status = ?", "PUBLISHED")
	if before != nil {
		q = q.Where("(published_at, id) < (?, ?)", before.PublishedAt, before.VideoID)
	}
	var videos []Video
	err := q.
		Order("published_at DESC, id DESC").
		Limit(limit + 1).
		Find(&videos).Error
	if err != nil {
		return nil, err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return []Video{}, nil
	}
	return videos, nil
}
