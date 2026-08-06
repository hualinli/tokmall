package interaction

import (
	"context"
	"errors"

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
