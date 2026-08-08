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
	q := r.feedQuery(ctx)
	if before != nil {
		q = q.Where("(videos.published_at, videos.id) < (?, ?)", before.PublishedAt, before.VideoID)
	}
	return r.findFeed(ctx, q, limit)
}

// ListPublishedByAuthor 作者主页的已发布作品：feed 查询 + author_id 过滤，
// 走 idx_videos_author 部分索引。
func (r *GormRepository) ListPublishedByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error) {
	q := r.feedQuery(ctx).Where("videos.author_id = ?", authorID)
	if before != nil {
		q = q.Where("(videos.published_at, videos.id) < (?, ?)", before.PublishedAt, before.VideoID)
	}
	return r.findFeed(ctx, q, limit)
}

// feedQuery 基座：已发布视频 + 作者信息（LEFT JOIN users，公开 read model）。
func (r *GormRepository) feedQuery(ctx context.Context) *gorm.DB {
	return r.db.WithContext(ctx).Table("videos").
		Select("videos.id, videos.author_id, videos.title, videos.object_key, "+
			"videos.cover_key, videos.duration_seconds, videos.published_at, "+
			"users.username, users.avatar_key").
		Joins("LEFT JOIN users ON users.id = videos.author_id").
		Where("videos.status = ?", "PUBLISHED")
}

func (r *GormRepository) findFeed(ctx context.Context, q *gorm.DB, limit int) ([]Video, error) {
	var videos []Video
	err := q.
		Order("videos.published_at DESC, videos.id DESC").
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

// LikeCounts 批量查点赞数：跨模块只读共享 likes 表，走 idx_likes_video 索引。
// like_count 不进 feed 缓存（点赞高频，避免缓存失效风暴），每次请求现查。
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
