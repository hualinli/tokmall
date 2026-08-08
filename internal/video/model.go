package video

import "time"

type Status string

const (
	StatusUploading     Status = "UPLOADING"
	StatusProcessing    Status = "PROCESSING"
	StatusPendingReview Status = "PENDING_REVIEW"
	StatusPublished     Status = "PUBLISHED"
	StatusFailed        Status = "FAILED"
	StatusBlocked       Status = "BLOCKED"
	StatusDeleted       Status = "DELETED"
)

// Video 对应 videos 表，schema 以 migrations/*.sql 为准。
// status 字段由状态机统一维护，不在此处直接赋值。
// Username/AvatarKey 来自 LEFT JOIN users（详情页作者信息）。
type Video struct {
	ID              int64      `gorm:"column:id;primaryKey"`
	AuthorID        int64      `gorm:"column:author_id"`
	Title           string     `gorm:"column:title"`
	ObjectKey       string     `gorm:"column:object_key"`
	CoverKey        *string    `gorm:"column:cover_key"`
	DurationSeconds int        `gorm:"column:duration_seconds"`
	Status          Status     `gorm:"column:status"`
	PublishedAt     *time.Time `gorm:"column:published_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
	Username        string     `gorm:"column:username"`
	AvatarKey       *string    `gorm:"column:avatar_key"`
}

func (Video) TableName() string {
	return "videos"
}

// UploadTicket 对应 upload_tickets 表：upload-url 发放、创建视频时核销。
type UploadTicket struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	UserID    int64     `gorm:"column:user_id"`
	ObjectKey string    `gorm:"column:object_key"`
	ExpiresAt time.Time `gorm:"column:expires_at"`
	Used      bool      `gorm:"column:used"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (UploadTicket) TableName() string {
	return "upload_tickets"
}
