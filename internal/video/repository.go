package video

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNotFound           = errors.New("record not found")
	ErrStatusConflict     = errors.New("status transition not allowed or already applied")
	ErrTicketInvalid      = errors.New("upload ticket missing, expired or used")
	ErrPlayEventNotFound  = errors.New("play event not found")
	ErrPlayEventReference = errors.New("play event references a missing record")
)

// PlayEventCreateResult 保留 INSERT 的事实结果：Inserted=true 表示本次请求
// 创建了新事件；否则 Existing 是数据库中已经存在的事实。仅凭 event_id 冲突
// 就直接返回成功会把不同业务载荷误判成重试，service 必须比较 Existing。
type PlayEventCreateResult struct {
	Inserted bool
	Existing *PlayEvent
}

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

// UpdateVideoStatus 带作者和前置状态的条件更新，防止越权及并发重复转换。
// 只有视频属于 authorID 且当前状态 == from 时才允许更新到 to，
// 否则返回 ErrStatusConflict。调用方应在 service 层先读取记录以区分
// 不存在、非作者和状态冲突；这里的 author_id 条件是最终的原子授权兜底。
func (r *GormRepository) UpdateVideoStatus(ctx context.Context, id, authorID int64, from, to Status, publishedAt *time.Time) error {
	res := r.db.WithContext(ctx).
		Model(&Video{}).
		Where("id = ? AND author_id = ? AND status = ?", id, authorID, string(from)).
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

// CreatePlayEvent 原子写入播放事件，并在 event_id 冲突后读取已有事实。
// PostgreSQL 的唯一索引负责并发仲裁：胜者 RowsAffected=1，竞争者等待胜者
// 提交后 RowsAffected=0，再读取同一个 event_id。这样 service 才能区分
// “相同请求重试”（成功）和“复用幂等键提交不同请求”（冲突）。
func (r *GormRepository) CreatePlayEvent(ctx context.Context, e *PlayEvent) (*PlayEventCreateResult, error) {
	res := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "event_id"}},
			DoNothing: true,
		}).Create(e)
	if res.Error != nil {
		return nil, translatePlayEventError(res.Error)
	}
	if res.RowsAffected > 0 {
		return &PlayEventCreateResult{Inserted: true}, nil
	}

	var existing PlayEvent
	if err := r.db.WithContext(ctx).
		Where("event_id = ?", e.EventID).
		Take(&existing).Error; err != nil {
		return nil, translatePlayEventError(err)
	}
	return &PlayEventCreateResult{Existing: &existing}, nil
}

// translatePlayEventError 把 repository 能识别的 ORM/数据库错误翻译成
// video 领域哨兵，避免 PostgreSQL 细节穿透到 service/handler。
func translatePlayEventError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrPlayEventNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrPlayEventReference
	}
	return err
}

// ConsumeTicket 原子核销 ticket：未被使用、未过期、属于该用户时标记 used，
// 并返回视频需要的元信息（如上传文件大小由 storage 提供，这里只核销）。
func (r *GormRepository) ConsumeTicket(ctx context.Context, userID int64, objectKey string) error {
	return consumeTicket(r.db.WithContext(ctx), userID, objectKey)
}

// ConsumeTicketAndCreateVideo 把凭证核销与视频落库放进同一事务。
// 分开写的话，"核销成功 + 建行失败"会把用户刚上传成功的对象变成孤儿：
// 凭证已烧掉、视频不存在，用户只能重新申请上传 URL 再传一次。
func (r *GormRepository) ConsumeTicketAndCreateVideo(ctx context.Context, userID int64, objectKey string, v *Video) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := consumeTicket(tx, userID, objectKey); err != nil {
			return err
		}
		return tx.Select("author_id", "title", "object_key", "cover_key", "duration_seconds", "status").Create(v).Error
	})
}

func consumeTicket(db *gorm.DB, userID int64, objectKey string) error {
	res := db.Model(&UploadTicket{}).
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
