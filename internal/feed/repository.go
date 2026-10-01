package feed

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

type GormRepository struct {
	db *gorm.DB
}

// 小关注集合用每作者有界索引查询，避免“关注作者很少/没有作品”时扫完整个
// 全局视频索引。超过此值改用全局时间索引过滤，避免大量 LATERAL index seek。
// 阈值来自 10 万视频合成数据的执行计划对比，后续在 F1 真实负载中复核。
const followingMergeMaxAuthors = 32

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

// ListByIDs 按 ID 批量取已发布视频。热榜的场景是"Redis 给出 ID 列表，
// 再补全卡片信息"，因此这里必须保持输入顺序无关（顺序由调用方决定），
// 并且过滤掉已删除/未发布的视频（物化快照里的条目可能已失效）。
func (r *GormRepository) ListByIDs(ctx context.Context, ids []int64) ([]Video, error) {
	if len(ids) == 0 {
		return []Video{}, nil
	}
	var videos []Video
	err := r.feedQuery(ctx).Where("videos.id IN ?", ids).Find(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
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

// ListFollowingPublished 在全部关注作者之间按时间游标取 limit+1 条。
// 先用 follows 主键索引计数：0 关注直接返回；小集合做每作者有界召回并归并，
// 大集合沿全局时间索引过滤。两种查询都返回相同的时间键顺序和 limit+1 语义。
func (r *GormRepository) ListFollowingPublished(ctx context.Context, userID int64, before *Cursor, limit int) ([]Video, error) {
	var followed int64
	if err := r.db.WithContext(ctx).Table("follows").Where("follower_id = ?", userID).Count(&followed).Error; err != nil {
		return nil, err
	}
	if followed == 0 {
		return []Video{}, nil
	}
	if followed <= followingMergeMaxAuthors {
		return r.listFollowingByAuthor(ctx, userID, before, limit)
	}
	return r.listFollowingByTime(ctx, userID, before, limit)
}

// 对每位作者只取游标之后的前 limit+1 条，再按全局时间键截断。
// 全局前 limit+1 条必包含在每位作者的前 limit+1 条中，因此不会再发生
// 旧版固定 K=5 的永久截断；候选规模最多为关注数 × (limit+1)。
func (r *GormRepository) listFollowingByAuthor(ctx context.Context, userID int64, before *Cursor, limit int) ([]Video, error) {
	inner := r.db.WithContext(ctx).Table("videos v2").
		Select("v2.id, v2.author_id, v2.title, v2.object_key, v2.cover_key, v2.duration_seconds, v2.published_at").
		Where("v2.author_id = f.followee_id AND v2.status = ?", "PUBLISHED")
	if before != nil {
		inner = inner.Where("(v2.published_at, v2.id) < (?, ?)", before.PublishedAt, before.VideoID)
	}
	inner = inner.Order("v2.published_at DESC, v2.id DESC").Limit(limit + 1)
	var videos []Video
	err := r.db.WithContext(ctx).Table("follows f").
		Select("v.id, v.author_id, v.title, v.object_key, v.cover_key, v.duration_seconds, v.published_at, u.username, u.avatar_key").
		Joins("JOIN LATERAL (?) AS v ON true", inner).
		Joins("LEFT JOIN users u ON u.id = v.author_id").
		Where("f.follower_id = ?", userID).
		Order("v.published_at DESC, v.id DESC").
		Limit(limit + 1).
		Scan(&videos).Error
	return videos, err
}

func (r *GormRepository) listFollowingByTime(ctx context.Context, userID int64, before *Cursor, limit int) ([]Video, error) {
	q := r.feedQuery(ctx).
		Where("EXISTS (SELECT 1 FROM follows WHERE follows.follower_id = ? AND follows.followee_id = videos.author_id)", userID)
	if before != nil {
		q = q.Where("(videos.published_at, videos.id) < (?, ?)", before.PublishedAt, before.VideoID)
	}
	return r.findFeed(ctx, q, limit)
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
