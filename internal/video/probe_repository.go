package video

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// 媒体校验的仓储实现。
//
// 取件用 `FOR UPDATE SKIP LOCKED`：多实例部署时每个 worker 拿到不同的行，
// 不会互相阻塞，也不需要额外的分布式锁（与 outbox 仓储同一套做法）。
// 但**锁只覆盖取件事务**：真正的探测（下载 + ffprobe + remux）可能几十秒，
// 不能把行锁持有那么久。因此取件只把行"标记为处理中"的语义交给
// SaveProbeResult 的条件更新来兜底——见下面 SaveProbeResult 的 WHERE。
func (r *GormRepository) ListPendingProbe(ctx context.Context, limit int) ([]ProbeCandidate, error) {
	if limit <= 0 {
		limit = 1
	}
	var rows []struct {
		ID              int64  `gorm:"column:id"`
		ObjectKey       string `gorm:"column:object_key"`
		DurationSeconds int    `gorm:"column:duration_seconds"`
	}
	err := r.db.WithContext(ctx).Raw(`
		SELECT id, object_key, duration_seconds
		FROM videos
		WHERE media_probe_status = 'PENDING'
		ORDER BY id ASC
		LIMIT ?`, limit).Scan(&rows).Error
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
func (r *GormRepository) SaveProbeResult(ctx context.Context, videoID int64, outcome ProbeOutcome) (bool, error) {
	if videoID <= 0 {
		return false, errors.New("invalid video id")
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

	query := r.db.WithContext(ctx).Model(&Video{}).
		Where("id = ? AND media_probe_status = ?", videoID, "PENDING")

	// 校验失败时把视频挡回待审：否则一个"上传时是坏文件"的视频
	// 可能已经处于 PUBLISHED 并被 Feed 缓存引用。
	if outcome.Status == "REJECTED" {
		query = query.Where("status <> ?", string(StatusPublished))
	}

	result := query.Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		// 可能原因：已被别的 worker 处理（正常），或视频已被删除（正常），
		// 或 REJECTED 撞上了"已经发布"（需要人工介入，记日志）。
		var status string
		if err := r.db.WithContext(ctx).Raw(`SELECT status FROM videos WHERE id = ?`, videoID).
			Scan(&status).Error; err == nil && status == string(StatusPublished) && outcome.Status == "REJECTED" {
			return false, fmt.Errorf("video %d is published but failed media validation: %s", videoID, outcome.Error)
		}
		return false, nil
	}
	return true, nil
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
var _ = gorm.ErrRecordNotFound
