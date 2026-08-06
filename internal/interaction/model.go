package interaction

import "time"

// Follow 对应 follows 表：PK(follower_id, followee_id) 由数据库保证幂等。
type Follow struct {
	FollowerID int64     `gorm:"column:follower_id"`
	FolloweeID int64     `gorm:"column:followee_id"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (Follow) TableName() string {
	return "follows"
}

// Like 对应 likes 表：PK(user_id, video_id) 由数据库保证幂等。
type Like struct {
	UserID    int64     `gorm:"column:user_id"`
	VideoID   int64     `gorm:"column:video_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (Like) TableName() string {
	return "likes"
}
