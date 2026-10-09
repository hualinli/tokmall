ALTER TABLE videos DROP CONSTRAINT videos_publish_probe_check;
DROP INDEX idx_videos_media_probe_pending;
ALTER TABLE videos DROP COLUMN probe_retry_at;
CREATE INDEX idx_videos_media_probe_pending ON videos (id) WHERE media_probe_status = 'PENDING';
-- 不自动重新发布曾被隔离的视频。
