package feed

import "time"

// Video 是 videos 表的只读投影（read model）：feed 只消费这几列，
// 不拥有视频事实数据，也不感知 video 模块的状态机。
// username/avatar_key 来自 LEFT JOIN users（公开作者信息）。
type Video struct {
	ID              int64      `gorm:"column:id"`
	AuthorID        int64      `gorm:"column:author_id"`
	Title           string     `gorm:"column:title"`
	ObjectKey       string     `gorm:"column:object_key"`
	CoverKey        *string    `gorm:"column:cover_key"`
	DurationSeconds int        `gorm:"column:duration_seconds"`
	PublishedAt     *time.Time `gorm:"column:published_at"`
	Username        string     `gorm:"column:username"`
	AvatarKey       *string    `gorm:"column:avatar_key"`
}

func (Video) TableName() string {
	return "videos"
}
