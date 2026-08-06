CREATE TABLE follows (
  follower_id BIGINT NOT NULL REFERENCES users(id),
  followee_id BIGINT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (follower_id, followee_id),
  CONSTRAINT ck_follow_no_self CHECK (follower_id <> followee_id)
);

CREATE INDEX idx_follows_followee
  ON follows (followee_id, follower_id);

CREATE TABLE likes (
  user_id BIGINT NOT NULL REFERENCES users(id),
  video_id BIGINT NOT NULL REFERENCES videos(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, video_id)
);

CREATE INDEX idx_likes_video ON likes (video_id, user_id);
