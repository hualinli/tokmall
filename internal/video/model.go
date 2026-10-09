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

// MediaProbeStatus records the outcome of asynchronous media validation.
type MediaProbeStatus string

const (
	ProbePending  MediaProbeStatus = "PENDING"
	ProbePassed   MediaProbeStatus = "PASSED"
	ProbeRejected MediaProbeStatus = "REJECTED"
	ProbeSkipped  MediaProbeStatus = "SKIPPED"
)

func (s MediaProbeStatus) AllowsPublish() bool { return s == ProbePassed || s == ProbeSkipped }

// Video 对应 videos 表，schema 以 migrations/*.sql 为准。
// status 字段由状态机统一维护，不在此处直接赋值。
// Username/AvatarKey 来自 LEFT JOIN users（详情页作者信息）。
type Video struct {
	MediaProbeStatus MediaProbeStatus `gorm:"column:media_probe_status"`
	ID               int64            `gorm:"column:id;primaryKey"`
	AuthorID         int64            `gorm:"column:author_id"`
	Title            string           `gorm:"column:title"`
	ObjectKey        string           `gorm:"column:object_key"`
	CoverKey         *string          `gorm:"column:cover_key"`
	DurationSeconds  int              `gorm:"column:duration_seconds"`
	Status           Status           `gorm:"column:status"`
	PublishedAt      *time.Time       `gorm:"column:published_at"`
	CreatedAt        time.Time        `gorm:"column:created_at"`
	UpdatedAt        time.Time        `gorm:"column:updated_at"`
	Username         string           `gorm:"column:username"`
	AvatarKey        *string          `gorm:"column:avatar_key"`
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

// PlayEvent 对应 play_events 表：播放事件流水。
// EventID 由客户端生成（接受标准 RFC 4122 UUID，当前 Web 客户端使用 v4，v7 也可），
// 唯一约束提供幂等去重（网络重试不重复计数）。
// 非去重语义：同一用户不同 event_id 的多次播放各自计数（产品层定义有效播放）。
type PlayEvent struct {
	EventID          string    `gorm:"column:event_id;primaryKey"`
	UserID           int64     `gorm:"column:user_id"`
	VideoID          int64     `gorm:"column:video_id"`
	SessionID        *string   `gorm:"column:session_id"`
	PlayedAt         time.Time `gorm:"column:played_at"`
	PlayedAtProvided bool      `gorm:"column:played_at_provided"`
	TraceID          *string   `gorm:"column:trace_id"`
	CreatedAt        time.Time `gorm:"column:created_at"`
}

func (PlayEvent) TableName() string {
	return "play_events"
}
