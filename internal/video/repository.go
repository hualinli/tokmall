package video

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

var ErrNotFound = errors.New("record not found")
var ErrStatusConflict = errors.New("status transition not allowed or already applied")
var ErrTicketInvalid = errors.New("upload ticket missing, expired or used")

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

func (r *GormRepository) CreateVideo(ctx context.Context, v *Video) error {
	return r.db.WithContext(ctx).Create(v).Error
}

func (r *GormRepository) FindVideoByID(ctx context.Context, id int64) (*Video, error) {
	var v Video
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&v).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &v, nil
}

// UpdateVideoStatus 带前置状态的条件更新，防止并发重复转换。
// 只有当前状态 == from 时才允许更新到 to，否则返回 ErrStatusConflict。
func (r *GormRepository) UpdateVideoStatus(ctx context.Context, id int64, from, to Status, publishedAt *time.Time) error {
	res := r.db.WithContext(ctx).
		Model(&Video{}).
		Where("id = ? AND status = ?", id, string(from)).
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

// ConsumeTicket 原子核销 ticket：未被使用、未过期、属于该用户时标记 used，
// 并返回视频需要的元信息（如上传文件大小由 storage 提供，这里只核销）。
func (r *GormRepository) ConsumeTicket(ctx context.Context, userID int64, objectKey string) error {
	res := r.db.WithContext(ctx).
		Model(&UploadTicket{}).
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
