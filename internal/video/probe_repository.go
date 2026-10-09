package video

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 待校验队列排除已删除视频；失败记录延后重试，让后续上传得到处理。
//
// 时间比较必须用**数据库时钟**（now()），不能把 Go 进程的 time.Now() 作为参数传进来：
// videos.probe_retry_at 首次写入走的是迁移里的 DEFAULT now()（PostgreSQL 时钟），
// 只有 DeferProbe 才会用 Go 时钟覆写它——这一列本来就在两个时钟域之间摇摆。
// 若这里拿应用主机时钟去比，两台机器只要有时钟偏差（容器、VM 休眠恢复、未校时的
// 开发机），新建视频就会对探测 worker 不可见，可见性延迟等于偏差量。
// 用同一个时钟源比较就从根上消掉了这个依赖。
func (r *GormRepository) ListPendingProbe(ctx context.Context, limit int) ([]ProbeCandidate, error) {
	if limit <= 0 {
		limit = 1
	}
	var rows []struct {
		ID              int64  `gorm:"column:id"`
		ObjectKey       string `gorm:"column:object_key"`
		DurationSeconds int    `gorm:"column:duration_seconds"`
	}
	err := r.db.WithContext(ctx).Model(&Video{}).
		Select("id, object_key, duration_seconds").
		Where("media_probe_status = ? AND status <> ? AND probe_retry_at <= now()", string(ProbePending), string(StatusDeleted)).
		Order("probe_retry_at ASC, id ASC").Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	candidates := make([]ProbeCandidate, 0, len(rows))
	for _, row := range rows {
		candidates = append(candidates, ProbeCandidate{
			VideoID:                 row.ID,
			ObjectKey:               row.ObjectKey,
			DeclaredDurationSeconds: row.DurationSeconds,
		})
	}
	return candidates, nil
}

// SaveProbeResult 写回校验结果。
//
// 条件更新 `WHERE media_probe_status = 'PENDING'` 是幂等的来源：
// 同一行被两个 worker 处理时，只有第一个能把状态从 PENDING 推走，
// 第二个得到 RowsAffected=0（返回 false，不视为错误）。
func (r *GormRepository) SaveProbeResult(ctx context.Context, videoID int64, outcome ProbeOutcome, beforeSave ...func() error) (bool, error) {
	if videoID <= 0 {
		return false, errors.New("invalid video id")
	}
	saved := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var v Video
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND media_probe_status = ? AND status <> ?", videoID, string(ProbePending), string(StatusDeleted)).Take(&v).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, action := range beforeSave {
			if err := action(); err != nil {
				return err
			}
		}
		now := time.Now().UTC()

		updates := map[string]any{
			"media_probe_status":      outcome.Status,
			"media_probe_error":       nullableProbeError(outcome.Error),
			"probed_at":               now,
			"probed_duration_seconds": nullableProbeFloat(outcome.DurationSeconds),
			"probed_width":            nullableProbeInt(outcome.Width),
			"probed_height":           nullableProbeInt(outcome.Height),
			"probed_video_codec":      nullableProbeString(outcome.VideoCodec),
			"faststart_applied":       outcome.FaststartApplied,
		}
		if outcome.Status == "SKIPPED" {
			// SKIPPED 的语义是"没校验"（工具缺失），因此不写 probed_at——
			// 数据库 CHECK 也要求这两者同真同假。
			delete(updates, "probed_at")
		}

		query := tx.Model(&Video{}).
			Where("id = ? AND media_probe_status = ? AND status <> ?", videoID, string(ProbePending), string(StatusDeleted))

		// 结果与下架同一条 UPDATE，数据库不会看到 REJECTED + PUBLISHED 的中间态。
		if outcome.Status == string(ProbeRejected) {
			updates["status"] = gorm.Expr("CASE WHEN status = ? THEN ? ELSE status END", string(StatusPublished), string(StatusBlocked))
		}

		result := query.Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		saved = result.RowsAffected == 1
		return nil
	})
	return saved, err
}

// DeferProbe delays transient failures rather than retaining a poison head of queue.
func (r *GormRepository) DeferProbe(ctx context.Context, videoID int64, retryAt time.Time) error {
	return r.db.WithContext(ctx).Model(&Video{}).
		Where("id = ? AND media_probe_status = ? AND status <> ?", videoID, string(ProbePending), string(StatusDeleted)).
		Update("probe_retry_at", retryAt.UTC()).Error
}

func nullableProbeError(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullableProbeString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullableProbeInt(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

func nullableProbeFloat(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	return &value
}

var _ ProbeRepository = (*GormRepository)(nil)
