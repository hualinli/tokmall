package video

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type objectDeleteJob struct {
	ObjectKey   string `gorm:"primaryKey"`
	AvailableAt time.Time
	Attempts    int
	LastError   *string
}

func (objectDeleteJob) TableName() string { return "object_delete_jobs" }

func enqueueObjectDelete(tx *gorm.DB, key string, at time.Time) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&objectDeleteJob{ObjectKey: key, AvailableAt: at.UTC()}).Error
}

// Deletion and its durable cleanup intent commit together.
func (r *GormRepository) deleteVideo(ctx context.Context, id, authorID int64, from Status) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var v Video
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND author_id = ? AND status = ?", id, authorID, string(from)).Take(&v).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrStatusConflict
		}
		if err != nil {
			return err
		}
		if err := tx.Model(&Video{}).Where("id = ?", id).Updates(map[string]any{"status": string(StatusDeleted), "published_at": nil}).Error; err != nil {
			return err
		}
		if err := enqueueObjectDelete(tx, v.ObjectKey, time.Now()); err != nil {
			return err
		}
		if v.CoverKey != nil {
			return enqueueObjectDelete(tx, *v.CoverKey, time.Now())
		}
		return nil
	})
}

type ObjectDeleter interface {
	DeleteObject(context.Context, string) error
}
type CleanupWorker struct {
	db      *gorm.DB
	storage ObjectDeleter
}

func NewCleanupWorker(db *gorm.DB, storage ObjectDeleter) *CleanupWorker {
	return &CleanupWorker{db: db, storage: storage}
}

func (w *CleanupWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "object cleanup failed; retry scheduled", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *CleanupWorker) RunOnce(ctx context.Context) error {
	// Expired unused uploads also need cleanup. The grace period ensures every
	// issued PUT has expired before we delete its staging object.
	if err := w.db.WithContext(ctx).Exec(`WITH expired AS (DELETE FROM upload_tickets WHERE expires_at < now() - interval '1 minute' RETURNING object_key, expires_at)
 INSERT INTO object_delete_jobs(object_key,available_at)
  SELECT 'staging/' || object_key, expires_at + interval '1 minute' FROM expired
  ON CONFLICT DO NOTHING`).Error; err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		found := false
		var deleteErr error
		err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var job objectDeleteJob
			err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("available_at <= ?", time.Now()).Order("available_at, object_key").Take(&job).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			deleteErr = w.storage.DeleteObject(deleteCtx, job.ObjectKey)
			cancel()
			if deleteErr != nil {
				return tx.Model(&job).Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": deleteErr.Error(), "available_at": time.Now().Add(time.Minute)}).Error
			}
			return tx.Delete(&job).Error
		})
		if err != nil {
			return err
		}
		if deleteErr != nil {
			return deleteErr
		}
		if !found {
			return nil
		}
	}
	return nil
}
