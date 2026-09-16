package interaction

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

var ErrUserNotFound = errors.New("user not found")

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

const (
	publishedVideoStatus = "PUBLISHED"
	activeUserStatus     = "ACTIVE"
)

// VideoIsInteractable 是 interaction 对 videos 表的最小只读投影。
// 未发布、已删除和不存在的视频统一不可互动，调用方不应据此区分具体状态。
func (r *GormRepository) VideoIsInteractable(ctx context.Context, videoID int64) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Table("videos").
		Where("id = ? AND status = ?", videoID, publishedVideoStatus).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// AddLike 幂等点赞：service 已做可见性检查，这里再用 EXISTS 把 PUBLISHED
// 条件放进写入语句，缩小 check-then-write 的 TOCTOU 窗口。
func (r *GormRepository) AddLike(ctx context.Context, userID, videoID int64) error {
	result := r.db.WithContext(ctx).Exec(`
		INSERT INTO likes (user_id, video_id)
		SELECT ?, ?
		WHERE EXISTS (
			SELECT 1 FROM videos
			WHERE id = ? AND status = 'PUBLISHED'
		)
		ON CONFLICT (user_id, video_id) DO NOTHING
	`, userID, videoID, videoID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		return nil
	}
	// RowsAffected=0 既可能是重复点赞，也可能是视频在 service 检查后变得不可见。
	visible, err := r.VideoIsInteractable(ctx, videoID)
	if err != nil {
		return err
	}
	if !visible {
		return ErrVideoNotFound
	}
	return nil
}

// RemoveLike 幂等取消：只有 PUBLISHED 视频允许改变关系；行不存在仍视为成功。
func (r *GormRepository) RemoveLike(ctx context.Context, userID, videoID int64) error {
	result := r.db.WithContext(ctx).Exec(`
		DELETE FROM likes l
		USING videos v
		WHERE l.user_id = ? AND l.video_id = v.id
		  AND v.id = ? AND v.status = 'PUBLISHED'
	`, userID, videoID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		return nil
	}
	visible, err := r.VideoIsInteractable(ctx, videoID)
	if err != nil {
		return err
	}
	if !visible {
		return ErrVideoNotFound
	}
	return nil
}

// AddFollow 幂等关注：目标用户必须在写入时仍处于 ACTIVE 且未软删；
// 唯一约束冲突在目标仍可交互时视为幂等成功。
func (r *GormRepository) AddFollow(ctx context.Context, followerID, followeeID int64) error {
	result := r.db.WithContext(ctx).Exec(`
		INSERT INTO follows (follower_id, followee_id)
		SELECT ?, ?
		WHERE EXISTS (
			SELECT 1 FROM users
			WHERE id = ? AND deleted_at IS NULL AND status = 'ACTIVE'
		)
		ON CONFLICT (follower_id, followee_id) DO NOTHING
	`, followerID, followeeID, followeeID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		return nil
	}
	exists, err := r.UserExists(ctx, followeeID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}
	return nil
}

// RemoveFollow 幂等取消关注。
func (r *GormRepository) RemoveFollow(ctx context.Context, followerID, followeeID int64) error {
	return r.db.WithContext(ctx).
		Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Delete(&Follow{}).Error
}

// UserExists 校验目标用户存在性（跨模块只读共享表，不依赖 user 模块类型）。
func (r *GormRepository) UserExists(ctx context.Context, userID int64) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Table("users").
		Where("id = ? AND deleted_at IS NULL AND status = ?", userID, activeUserStatus).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// FollowEdge 关注关系行 + 对方公开资料（JOIN users）。
type FollowEdge struct {
	UserID    int64     `gorm:"column:user_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
	Username  string    `gorm:"column:username"`
	Bio       string    `gorm:"column:bio"`
	AvatarKey *string   `gorm:"column:avatar_key"`
}

// ListFollowing 某人关注了谁：按 (created_at, followee_id) 倒序键集分页。
// 注意：无 (follower_id, created_at) 索引，个人关注量级小，排序开销可接受。
func (r *GormRepository) ListFollowing(ctx context.Context, userID int64, before *Cursor, limit int) ([]FollowEdge, error) {
	q := r.db.WithContext(ctx).Table("follows f").
		Select("f.followee_id AS user_id, f.created_at, u.username, u.bio, u.avatar_key").
		Joins("JOIN users u ON u.id = f.followee_id AND u.deleted_at IS NULL AND u.status = 'ACTIVE'").
		Where("f.follower_id = ?", userID)
	if before != nil {
		q = q.Where("(f.created_at, f.followee_id) < (?, ?)", before.CreatedAt, before.UserID)
	}
	return r.findFollowEdges(ctx, q, "f.created_at DESC, f.followee_id DESC", limit)
}

// ListFollowers 谁关注了某人。
func (r *GormRepository) ListFollowers(ctx context.Context, userID int64, before *Cursor, limit int) ([]FollowEdge, error) {
	q := r.db.WithContext(ctx).Table("follows f").
		Select("f.follower_id AS user_id, f.created_at, u.username, u.bio, u.avatar_key").
		Joins("JOIN users u ON u.id = f.follower_id AND u.deleted_at IS NULL AND u.status = 'ACTIVE'").
		Where("f.followee_id = ?", userID)
	if before != nil {
		q = q.Where("(f.created_at, f.follower_id) < (?, ?)", before.CreatedAt, before.UserID)
	}
	return r.findFollowEdges(ctx, q, "f.created_at DESC, f.follower_id DESC", limit)
}

func (r *GormRepository) findFollowEdges(ctx context.Context, q *gorm.DB, order string, limit int) ([]FollowEdge, error) {
	var edges []FollowEdge
	err := q.Order(order).Limit(limit + 1).Scan(&edges).Error
	if err != nil {
		return nil, err
	}
	return edges, nil
}

// IsFollowingMany 批量查 viewer 对一批用户的关注状态（列表页"回关"按钮）。
func (r *GormRepository) IsFollowingMany(ctx context.Context, viewerID int64, userIDs []int64) (map[int64]bool, error) {
	result := make(map[int64]bool, len(userIDs))
	if viewerID <= 0 || len(userIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		FolloweeID int64
	}
	err := r.db.WithContext(ctx).Table("follows f").
		Select("followee_id").
		Joins("JOIN users u ON u.id = f.followee_id AND u.deleted_at IS NULL AND u.status = 'ACTIVE'").
		Where("follower_id = ? AND followee_id IN ?", viewerID, userIDs).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.FolloweeID] = true
	}
	return result, nil
}
