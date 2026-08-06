CREATE TABLE videos (
  id BIGSERIAL PRIMARY KEY,
  author_id BIGINT NOT NULL REFERENCES users(id),
  title TEXT NOT NULL,
  object_key TEXT NOT NULL,
  cover_key TEXT,
  duration_seconds INT NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  published_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_videos_feed
  ON videos (published_at DESC, id DESC)
  WHERE status = 'PUBLISHED';

CREATE INDEX idx_videos_author
  ON videos (author_id, published_at DESC)
  WHERE status = 'PUBLISHED';

CREATE TABLE upload_tickets (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id),
  object_key TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  used BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_upload_tickets_user
  ON upload_tickets (user_id, object_key);
