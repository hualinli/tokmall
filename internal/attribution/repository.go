package attribution

import (
	"context"

	"gorm.io/gorm"
)

type GormRepository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *GormRepository { return &GormRepository{db: db} }

// One read-only snapshot keeps totals and breakdowns internally consistent
// while payments are being committed concurrently. An idempotent event
// projection can replace this query when measurement justifies it.
func (r *GormRepository) Sales(ctx context.Context, authorID int64) (*Sales, error) {
	result := &Sales{AuthorID: authorID, ByVideo: []Slice{}, ByProduct: []Slice{}}
	base := `FROM orders o
		JOIN LATERAL (SELECT sum(quantity) AS units FROM order_items WHERE order_id = o.id) i ON TRUE
		WHERE o.source_author_id = ? AND o.paid_at IS NOT NULL`
	metrics := `count(*) AS order_count, count(DISTINCT o.user_id) AS buyer_count,
		coalesce(sum(i.units), 0) AS units_sold,
		coalesce(sum(o.total_amount), 0) AS gmv,
		count(*) FILTER (WHERE o.status = 'REFUNDED') AS refunded_orders,
		coalesce(sum(o.total_amount) FILTER (WHERE o.status = 'REFUNDED'), 0) AS refunded_amount`
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error; err != nil {
			return err
		}
		if err := tx.Raw("SELECT "+metrics+" "+base, authorID).Scan(result).Error; err != nil {
			return err
		}
		if err := tx.Raw("SELECT o.source_video_id AS id, "+metrics+" "+base+
			" GROUP BY o.source_video_id ORDER BY gmv DESC, id", authorID).Scan(&result.ByVideo).Error; err != nil {
			return err
		}
		return tx.Raw("SELECT o.source_product_id AS id, "+metrics+" "+base+
			" GROUP BY o.source_product_id ORDER BY gmv DESC, id", authorID).Scan(&result.ByProduct).Error
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
