package feed

import (
	"context"

	"gorm.io/gorm"
)

// ProductCard is a feed-owned read model. The cover key never leaves the API.
type ProductCard struct {
	ProductID     int64   `json:"product_id"`
	Title         string  `json:"title"`
	MinPrice      int64   `json:"min_price"`
	ProductStatus string  `json:"product_status"`
	CoverURL      *string `json:"cover_url"`
	SortOrder     int32   `json:"sort_order"`
}

type productCardRow struct {
	VideoID       int64  `gorm:"column:video_id"`
	ProductID     int64  `gorm:"column:product_id"`
	Title         string `gorm:"column:title"`
	MinPrice      int64  `gorm:"column:min_price"`
	ProductStatus string `gorm:"column:product_status"`
	CoverKey      string `gorm:"column:cover_key"`
	SortOrder     int32  `gorm:"column:sort_order"`
}

type ProductCardReader interface {
	ListProductCards(ctx context.Context, videoIDs []int64) ([]productCardRow, error)
}

type GormProductCardReader struct{ db *gorm.DB }

func NewProductCardReader(db *gorm.DB) *GormProductCardReader { return &GormProductCardReader{db: db} }

func (r *GormProductCardReader) ListProductCards(ctx context.Context, videoIDs []int64) ([]productCardRow, error) {
	if len(videoIDs) == 0 {
		return nil, nil
	}
	var rows []productCardRow
	err := r.db.WithContext(ctx).Raw(`
		SELECT vp.video_id, p.id AS product_id, p.title,
		       coalesce(min(s.price), 0) AS min_price,
		       p.status AS product_status, p.cover_key, vp.sort_order
		FROM video_products vp
		JOIN videos v ON v.id = vp.video_id AND v.status = 'PUBLISHED'
		JOIN products p ON p.id = vp.product_id AND p.status <> 'DELETED'
		LEFT JOIN skus s ON s.product_id = p.id AND s.status = 'ACTIVE'
		WHERE vp.video_id IN ? AND vp.status = 'ACTIVE'
		GROUP BY vp.video_id, vp.id, p.id
		ORDER BY vp.video_id, vp.sort_order, vp.id`, videoIDs).Scan(&rows).Error
	return rows, err
}
