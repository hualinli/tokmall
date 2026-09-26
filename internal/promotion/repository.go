package promotion

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNotFound    = errors.New("promotion resource not found")
	ErrActiveLimit = errors.New("video active product limit reached")
)

type GormRepository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *GormRepository { return &GormRepository{db: db} }

// Upsert 在同一事务中锁住资格事实再写关系，避免校验后视频被下架或商品被
// 改状态。唯一键使重复挂载只会更新/重新启用原关系。
func (r *GormRepository) Upsert(ctx context.Context, authorID, videoID, productID int64, sortOrder int32) (*VideoProduct, error) {
	var relation VideoProduct
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireEligible(tx, authorID, videoID, productID); err != nil {
			return err
		}
		relation = VideoProduct{VideoID: videoID, AuthorID: authorID, ProductID: productID, Status: StatusActive, SortOrder: sortOrder}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "video_id"}, {Name: "product_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"author_id": authorID, "status": string(StatusActive),
				"sort_order": sortOrder, "updated_at": gorm.Expr("now()"),
			}),
		}).Create(&relation).Error
	})
	return &relation, translateError(err)
}

func (r *GormRepository) UpdateSort(ctx context.Context, authorID, videoID, productID int64, sortOrder int32) (*VideoProduct, error) {
	return r.update(ctx, authorID, videoID, productID, map[string]any{"sort_order": sortOrder, "updated_at": gorm.Expr("now()")})
}

func (r *GormRepository) Disable(ctx context.Context, authorID, videoID, productID int64) (*VideoProduct, error) {
	var relation VideoProduct
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Unlink must remain possible after a product goes off sale or a video
		// is blocked. Only the original video author may change the relation.
		if err := requireVideoOwner(tx, authorID, videoID); err != nil {
			return err
		}
		result := tx.Model(&VideoProduct{}).
			Where("video_id = ? AND product_id = ? AND author_id = ?", videoID, productID, authorID).
			Updates(map[string]any{"status": string(StatusDisabled), "updated_at": gorm.Expr("now()")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return tx.Where("video_id = ? AND product_id = ? AND author_id = ?", videoID, productID, authorID).Take(&relation).Error
	})
	return &relation, translateError(err)
}

func requireVideoOwner(tx *gorm.DB, authorID, videoID int64) error {
	var row struct {
		ID int64 `gorm:"column:id"`
	}
	if err := tx.Raw("SELECT id FROM videos WHERE id = ? AND author_id = ? FOR SHARE", videoID, authorID).Scan(&row).Error; err != nil {
		return err
	}
	if row.ID == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *GormRepository) update(ctx context.Context, authorID, videoID, productID int64, changes map[string]any) (*VideoProduct, error) {
	var relation VideoProduct
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireEligible(tx, authorID, videoID, productID); err != nil {
			return err
		}
		result := tx.Model(&VideoProduct{}).
			Where("video_id = ? AND product_id = ? AND author_id = ?", videoID, productID, authorID).
			Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return tx.Where("video_id = ? AND product_id = ? AND author_id = ?", videoID, productID, authorID).Take(&relation).Error
	})
	return &relation, translateError(err)
}

func (r *GormRepository) ListActiveCards(ctx context.Context, videoID int64) ([]ProductCard, error) {
	var cards []ProductCard
	err := r.db.WithContext(ctx).Raw(`
		SELECT p.id AS product_id, p.title, coalesce(min(s.price), 0) AS min_price,
		       p.status AS product_status, vp.sort_order
		FROM video_products vp
		JOIN videos v ON v.id = vp.video_id AND v.status = 'PUBLISHED'
		JOIN products p ON p.id = vp.product_id AND p.status <> 'DELETED'
		LEFT JOIN skus s ON s.product_id = p.id AND s.status = 'ACTIVE'
		WHERE vp.video_id = ? AND vp.status = 'ACTIVE'
		GROUP BY p.id, p.title, vp.sort_order, vp.id
		ORDER BY vp.sort_order ASC, vp.id ASC`, videoID).Scan(&cards).Error
	return cards, err
}

func requireEligible(tx *gorm.DB, authorID, videoID, productID int64) error {
	var row struct {
		VideoID int64 `gorm:"column:video_id"`
	}
	err := tx.Raw(`
		SELECT v.id AS video_id
		FROM videos v
		JOIN products p ON p.id = ?
		WHERE v.id = ? AND v.author_id = ? AND v.status = 'PUBLISHED'
		  AND p.merchant_id = ? AND p.status = 'ON_SALE'
		FOR SHARE OF v, p`, productID, videoID, authorID, authorID).Scan(&row).Error
	if err != nil {
		return err
	}
	if row.VideoID == 0 {
		return ErrNotFound
	}
	return nil
}

func translateError(err error) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "video_products_active_limit_check" {
		return ErrActiveLimit
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
