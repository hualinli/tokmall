package product

import (
	"context"
	"fmt"
)

// EachProductID 按主键顺序分批遍历"仍然存在"的商品 ID，供布隆过滤器全量重建。
// 只排除软删（DELETED）：下架商品仍要进过滤器，否则商家查看自己的下架商品
// 会被判成"一定不存在"。
func (r *GormRepository) EachProductID(ctx context.Context, batch int, fn func(id int64) error) error {
	if batch <= 0 {
		batch = 1000
	}
	lastID := int64(0)
	for {
		var ids []int64
		err := r.db.WithContext(ctx).Model(&Product{}).
			Where("id > ? AND status <> ?", lastID, string(ProductStatusDeleted)).
			Order("id ASC").Limit(batch).Pluck("id", &ids).Error
		if err != nil {
			return fmt.Errorf("list product ids: %w", err)
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
