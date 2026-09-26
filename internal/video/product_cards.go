package video

import (
	"context"

	"gorm.io/gorm"
)

type ProductCard struct {
	ProductID int64   `json:"product_id"`
	Title     string  `json:"title"`
	MinPrice  int64   `json:"min_price"`
	CoverURL  *string `json:"cover_url"`
	SortOrder int32   `json:"sort_order"`
}

type productCardRow struct {
	ProductID int64  `gorm:"column:product_id"`
	Title     string `gorm:"column:title"`
	MinPrice  int64  `gorm:"column:min_price"`
	CoverKey  string `gorm:"column:cover_key"`
	SortOrder int32  `gorm:"column:sort_order"`
}

type ProductCardReader interface {
	ListProductCards(ctx context.Context, videoID int64) ([]productCardRow, error)
}

type GormProductCardReader struct{ db *gorm.DB }

func NewProductCardReader(db *gorm.DB) *GormProductCardReader { return &GormProductCardReader{db: db} }

func (r *GormProductCardReader) ListProductCards(ctx context.Context, videoID int64) ([]productCardRow, error) {
	var rows []productCardRow
	err := r.db.WithContext(ctx).Raw(`
		SELECT p.id AS product_id, p.title, min(s.price) AS min_price,
		       p.cover_key, vp.sort_order
		FROM video_products vp
		JOIN products p ON p.id = vp.product_id AND p.status = 'ON_SALE'
		JOIN skus s ON s.product_id = p.id AND s.status = 'ACTIVE'
		WHERE vp.video_id = ? AND vp.status = 'ACTIVE'
		GROUP BY vp.id, p.id
		ORDER BY vp.sort_order, vp.id`, videoID).Scan(&rows).Error
	return rows, err
}
