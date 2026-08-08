-- 作者视频列表（管理视图）：按创建时间倒序，键集游标 (created_at, id)。
-- 公开视图（仅 PUBLISHED）走已有的 idx_videos_author。
CREATE INDEX idx_videos_author_created
  ON videos (author_id, created_at DESC, id DESC);
