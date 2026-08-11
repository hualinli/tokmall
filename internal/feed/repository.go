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

// ListFollowingIDs 读某人的关注列表（跨模块只读 follows 表，不依赖 interaction 类型）。
// 走 follows 主键索引 (follower_id, followee_id)。V1 不缓存关注列表：
// 单次索引查询 + 千行量级，缓存收益小；关注数增长后（M6 演进）再缓存。
func (r *GormRepository) ListFollowingIDs(ctx context.Context, userID int64) ([]int64, error) {
	var ids []int64
	err := r.db.WithContext(ctx).Table("follows").
		Where("follower_id = ?", userID).
		Order("created_at DESC").
		Pluck("followee_id", &ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ListFollowingCandidates 关注 Feed 的召回：对每个关注作者取最近 perAuthor 条已发布视频。
// 窗口函数（ROW_NUMBER PARTITION BY author）一次 SQL 完成"每作者 TOP K"，
// 避免逐作者查询的 N+1。
// 召回不排序不截断：排序交给 service 的 Ranker（M8：候选召回与排序分离），
// 分页截断由 assemble 完成。游标下推到窗口内：翻页只看游标之前的视频。
func (r *GormRepository) ListFollowingCandidates(ctx context.Context, userID int64, before *Cursor, perAuthor int) ([]Video, error) {
	inner := r.db.WithContext(ctx).Table("videos v").
		Select("v.id, v.author_id, v.title, v.object_key, v.cover_key, v.duration_seconds, "+
			"v.published_at, u.username, u.avatar_key, "+
			"ROW_NUMBER() OVER (PARTITION BY v.author_id "+
			"ORDER BY v.published_at DESC, v.id DESC) AS rn").
		Joins("JOIN users u ON u.id = v.author_id").
		Where("v.author_id IN (?)", r.db.Table("follows").
			Select("followee_id").Where("follower_id = ?", userID)).
		Where("v.status = ?", "PUBLISHED")
	if before != nil {
		inner = inner.Where("(v.published_at, v.id) < (?, ?)", before.PublishedAt, before.VideoID)
	}
	var videos []Video
	err := r.db.WithContext(ctx).Table("(?) AS t", inner).
		Select("t.id, t.author_id, t.title, t.object_key, t.cover_key, t.duration_seconds, "+
			"t.published_at, t.username, t.avatar_key").
		Where("t.rn <= ?", perAuthor).
		Scan(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
}

// PlayCounts 批量查播放数（跨模块只读 play_events 表，走 idx_play_events_video_time 索引）。
// 供 HotRanker 计算热度分数；TimeRanker 不调用（NeedsCounts 控制）。
func (r *GormRepository) PlayCounts(ctx context.Context, videoIDs []int64) (map[int64]int64, error) {
	if len(videoIDs) == 0 {
		return map[int64]int64{}, nil
	}
	var rows []struct {
		VideoID int64
		Count   int64
	}
	err := r.db.WithContext(ctx).Table("play_events").
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
