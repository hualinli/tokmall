DROP INDEX IF EXISTS idx_videos_media_probe_pending;
ALTER TABLE videos DROP CONSTRAINT IF EXISTS videos_rejected_not_published_check;
ALTER TABLE videos DROP CONSTRAINT IF EXISTS videos_probe_state_check;
ALTER TABLE videos DROP CONSTRAINT IF EXISTS videos_probed_duration_check;
ALTER TABLE videos DROP CONSTRAINT IF EXISTS videos_media_probe_status_check;
ALTER TABLE videos
    DROP COLUMN IF EXISTS faststart_applied,
    DROP COLUMN IF EXISTS probed_at,
    DROP COLUMN IF EXISTS probed_video_codec,
    DROP COLUMN IF EXISTS probed_height,
    DROP COLUMN IF EXISTS probed_width,
    DROP COLUMN IF EXISTS probed_duration_seconds,
    DROP COLUMN IF EXISTS media_probe_error,
    DROP COLUMN IF EXISTS media_probe_status;
