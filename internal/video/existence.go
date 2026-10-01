package video

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// EachVideoID 按主键顺序分批遍历"仍然存在"的视频 ID，供布隆过滤器全量重建。
//
// 只排除软删（DELETED）：未发布视频也必须进过滤器，否则作者查看自己的
// 未发布视频会被布隆过滤器判成"一定不存在"——这是不能接受的假阴性。
func (r *GormRepository) EachVideoID(ctx context.Context, batch int, fn func(id int64) error) error {
	if batch <= 0 {
		batch = 1000
	}
	lastID := int64(0)
	for {
		var ids []int64
		err := r.db.WithContext(ctx).Model(&Video{}).
			Where("id > ? AND status <> ?", lastID, string(StatusDeleted)).
			Order("id ASC").Limit(batch).Pluck("id", &ids).Error
		if err != nil {
			return fmt.Errorf("list video ids: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err := fn(id); err != nil {
				return err
			}
		}
		lastID = ids[len(ids)-1]
		if len(ids) < batch {
			return nil
		}
	}
}

var _ = gorm.ErrRecordNotFound
