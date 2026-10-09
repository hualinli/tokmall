ALTER TABLE videos ADD COLUMN probe_retry_at TIMESTAMPTZ NOT NULL DEFAULT now();
-- 修复旧实现允许未校验发布的存量；历史 SKIPPED 视频仍可发布。
UPDATE videos SET status = 'PENDING_REVIEW', published_at = NULL
    WHERE status = 'PUBLISHED' AND media_probe_status = 'PENDING';
ALTER TABLE videos ADD CONSTRAINT videos_publish_probe_check
    CHECK (status <> 'PUBLISHED' OR media_probe_status IN ('PASSED', 'SKIPPED'));
DROP INDEX idx_videos_media_probe_pending;
CREATE INDEX idx_videos_media_probe_pending ON videos (probe_retry_at, id)
    WHERE media_probe_status = 'PENDING' AND status <> 'DELETED';
