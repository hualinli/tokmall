package feed

import (
	"context"

	"gorm.io/gorm"
)

// DynamicReader 是 feed 消费侧的只读投影：把每页两个动态读合成一次往返。
// 结果仍属于当前请求，不进入共享页面缓存。
type DynamicReader interface {
	ReadDynamic(ctx context.Context, videoIDs []int64) (map[int64]int64, []productCardRow, error)
}

type GormDynamicReader struct{ db *gorm.DB }

func NewDynamicReader(db *gorm.DB) *GormDynamicReader { return &GormDynamicReader{db: db} }

func (r *GormDynamicReader) ReadDynamic(ctx context.Context, videoIDs []int64) (map[int64]int64, []productCardRow, error) {
	counts := make(map[int64]int64, len(videoIDs))
	if len(videoIDs) == 0 {
		return counts, nil, nil
	}
	var rows []struct {
		VideoID       int64
		LikeCount     int64
		ProductID     *int64
		Title         string
		MinPrice      int64
		ProductStatus string
		CoverKey      string
		SortOrder     int32
	}
	// 点赞先聚合，商品卡内的 SKU 也先聚合，再连接两个结果。
	// 直接把 likes、商品、SKU 一起 JOIN 会把点赞数乘以挂载/SKU 数量。
	err := r.db.WithContext(ctx).Raw(`
		WITH counts AS (
			SELECT video_id, count(*) AS like_count FROM likes
			WHERE video_id IN ? GROUP BY video_id
		), cards AS (
			SELECT vp.video_id, vp.id AS relation_id, p.id AS product_id, p.title,
			       coalesce(min(s.price), 0) AS min_price,
			       p.status AS product_status, p.cover_key, vp.sort_order
			FROM video_products vp
			JOIN videos v ON v.id = vp.video_id AND v.status = 'PUBLISHED'
			JOIN products p ON p.id = vp.product_id AND p.status <> 'DELETED'
			LEFT JOIN skus s ON s.product_id = p.id AND s.status = 'ACTIVE'
			WHERE vp.video_id IN ? AND vp.status = 'ACTIVE'
			GROUP BY vp.video_id, vp.id, p.id
		)
		SELECT v.id AS video_id, coalesce(c.like_count, 0) AS like_count,
		       p.product_id, coalesce(p.title, '') AS title,
		       coalesce(p.min_price, 0) AS min_price,
		       coalesce(p.product_status, '') AS product_status,
		       coalesce(p.cover_key, '') AS cover_key,
		       coalesce(p.sort_order, 0) AS sort_order
		FROM videos v
		LEFT JOIN counts c ON c.video_id = v.id
		LEFT JOIN cards p ON p.video_id = v.id
		WHERE v.id IN ?
		ORDER BY v.id, p.sort_order, p.relation_id`, videoIDs, videoIDs, videoIDs).Scan(&rows).Error
	if err != nil {
		return nil, nil, err
	}
	cards := make([]productCardRow, 0, len(rows))
	for _, row := range rows {
		counts[row.VideoID] = row.LikeCount
		if row.ProductID != nil {
			cards = append(cards, productCardRow{VideoID: row.VideoID, ProductID: *row.ProductID,
				Title: row.Title, MinPrice: row.MinPrice, ProductStatus: row.ProductStatus,
				CoverKey: row.CoverKey, SortOrder: row.SortOrder})
		}
	}
	return counts, cards, nil
}
