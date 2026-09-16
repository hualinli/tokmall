package user

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

type column string

const (
	colID       column = "id"
	colUsername column = "username"
	colEmail    column = "email"
)

var ErrEmailTaken = errors.New("email already taken")
var ErrUsernameTaken = errors.New("username already taken")
var ErrNotFound = errors.New("record not found")

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

func (r *GormRepository) Create(ctx context.Context, u *User) error {
	if err := r.db.WithContext(ctx).Create(u).Error; err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			switch pgErr.ConstraintName {
			case "uq_users_email":
				return ErrEmailTaken
			case "uq_users_username":
				return ErrUsernameTaken
			}
		}
		return err
	}
	return nil
}

func (r *GormRepository) FindByID(ctx context.Context, id int64) (*User, error) {
	return r.findOne(ctx, colID, id)
}

func (r *GormRepository) FindByUsername(ctx context.Context, username string) (*User, error) {
	return r.findOne(ctx, colUsername, username)
}

func (r *GormRepository) FindByEmail(ctx context.Context, email string) (*User, error) {
	return r.findOne(ctx, colEmail, email)
}

func (r *GormRepository) findOne(ctx context.Context, col column, value any) (*User, error) {
	var u User
	if err := r.db.WithContext(ctx).Where(string(col)+" = ?", value).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// FollowCounts 关注/粉丝计数：跨模块只读共享 follows 表（各自的 read model），
// 两条子查询走 PK 与 idx_follows_followee 索引。
func (r *GormRepository) FollowCounts(ctx context.Context, userID int64) (following, followers int64, err error) {
	var counts struct {
		Following int64
		Followers int64
	}
	err = r.db.WithContext(ctx).Raw(`
		SELECT
			(SELECT count(*)
			 FROM follows f
			 JOIN users u ON u.id = f.followee_id
			 WHERE f.follower_id = ?
			   AND u.deleted_at IS NULL
			   AND u.status = 'ACTIVE') AS following,
			(SELECT count(*)
			 FROM follows f
			 JOIN users u ON u.id = f.follower_id
			 WHERE f.followee_id = ?
			   AND u.deleted_at IS NULL
			   AND u.status = 'ACTIVE') AS followers`,
		userID, userID).Scan(&counts).Error
	if err != nil {
		return 0, 0, err
	}
	return counts.Following, counts.Followers, nil
}

// IsFollowing 判断 viewer 是否已关注目标。
func (r *GormRepository) IsFollowing(ctx context.Context, followerID, followeeID int64) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Table("follows").
		Joins("JOIN users u ON u.id = follows.followee_id AND u.deleted_at IS NULL AND u.status = 'ACTIVE'").
		Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// UpdateProfile 更新资料：nil 字段跳过（局部更新语义）。
func (r *GormRepository) UpdateProfile(ctx context.Context, userID int64, bio *string, avatarKey *string) error {
	updates := map[string]any{}
	if bio != nil {
		updates["bio"] = *bio
	}
	if avatarKey != nil {
		updates["avatar_key"] = *avatarKey
	}
	if len(updates) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&User{}).
		Where("id = ?", userID).
		Updates(updates).Error
}
