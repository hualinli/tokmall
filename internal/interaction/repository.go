package interaction

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrUserNotFound = errors.New("user not found")

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

// AddLike 幂等点赞：唯一约束冲突时静默忽略，永远不报错。
func (r *GormRepository) AddLike(ctx context.Context, userID, videoID int64) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&Like{UserID: userID, VideoID: videoID}).Error
}

// RemoveLike 幂等取消：行不存在时删除 0 行，同样不报错。
func (r *GormRepository) RemoveLike(ctx context.Context, userID, videoID int64) error {
	return r.db.WithContext(ctx).
		Where("user_id = ? AND video_id = ?", userID, videoID).
		Delete(&Like{}).Error
}

// AddFollow 幂等关注：唯一约束冲突时静默忽略。
func (r *GormRepository) AddFollow(ctx context.Context, followerID, followeeID int64) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&Follow{FollowerID: followerID, FolloweeID: followeeID}).Error
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
	err := r.db.WithContext(ctx).Table("users").Where("id = ?", userID).Count(&count).Error
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
		Joins("JOIN users u ON u.id = f.followee_id").
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
		Joins("JOIN users u ON u.id = f.follower_id").
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
	err := r.db.WithContext(ctx).Table("follows").
		Select("followee_id").
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
