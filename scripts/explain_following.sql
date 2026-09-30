-- 关注 Feed 完整时间线查询计划：临时表造 1000 作者、10 万视频，分别分析
-- 1000 / 10 / 0 关注及稀疏分布，比较全局索引与每作者归并。
-- 只创建 TEMP 表，不修改项目业务表；结果不等同真实 QPS。
-- 用法：docker exec -i tokmall-postgres psql -U tokmall -d tokmall < scripts/explain_following.sql
\set ON_ERROR_STOP on
CREATE TEMP TABLE users (id bigint PRIMARY KEY, username text NOT NULL, avatar_key text);
CREATE TEMP TABLE videos (id bigint PRIMARY KEY, author_id bigint NOT NULL, title text NOT NULL, object_key text NOT NULL, cover_key text, duration_seconds integer NOT NULL, published_at timestamptz NOT NULL, status text NOT NULL);
CREATE INDEX idx_videos_feed ON videos (published_at DESC, id DESC) WHERE status = 'PUBLISHED';
CREATE INDEX idx_videos_author ON videos (author_id, published_at DESC) WHERE status = 'PUBLISHED';
CREATE TEMP TABLE follows (follower_id bigint NOT NULL, followee_id bigint NOT NULL, PRIMARY KEY (follower_id, followee_id));
INSERT INTO users SELECT i, 'author_' || i, NULL FROM generate_series(1,1000) i;
INSERT INTO videos SELECT i, 1 + (i % 1000), 'video_' || i, 'videos/' || i || '.mp4', NULL, 30, timestamp with time zone '2026-09-26 00:00:00+00' - i * interval '1 second', 'PUBLISHED' FROM generate_series(1,100000) i;
INSERT INTO follows SELECT 0, i FROM generate_series(1,1000) i;
ANALYZE users;
ANALYZE videos;
ANALYZE follows;
\echo 'CASE 1000 follows / first page'
EXPLAIN (ANALYZE, BUFFERS) SELECT videos.id, users.username FROM videos LEFT JOIN users ON users.id = videos.author_id WHERE videos.status = 'PUBLISHED' AND EXISTS (SELECT 1 FROM follows f0 WHERE f0.follower_id = 0) AND EXISTS (SELECT 1 FROM follows WHERE follows.follower_id = 0 AND follows.followee_id = videos.author_id) ORDER BY videos.published_at DESC, videos.id DESC LIMIT 21;
\echo 'LATERAL comparison'
EXPLAIN (ANALYZE, BUFFERS) SELECT v.id, u.username FROM follows f JOIN LATERAL (SELECT id, author_id, published_at FROM videos WHERE author_id = f.followee_id AND status = 'PUBLISHED' ORDER BY published_at DESC, id DESC LIMIT 21) v ON true LEFT JOIN users u ON u.id = v.author_id WHERE f.follower_id = 0 ORDER BY v.published_at DESC, v.id DESC LIMIT 21;
DELETE FROM follows WHERE followee_id > 10;
ANALYZE follows;
\echo 'CASE 10 follows / first page'
EXPLAIN (ANALYZE, BUFFERS) SELECT videos.id, users.username FROM videos LEFT JOIN users ON users.id = videos.author_id WHERE videos.status = 'PUBLISHED' AND EXISTS (SELECT 1 FROM follows f0 WHERE f0.follower_id = 0) AND EXISTS (SELECT 1 FROM follows WHERE follows.follower_id = 0 AND follows.followee_id = videos.author_id) ORDER BY videos.published_at DESC, videos.id DESC LIMIT 21;
\echo 'LATERAL comparison'
EXPLAIN (ANALYZE, BUFFERS) SELECT v.id, u.username FROM follows f JOIN LATERAL (SELECT id, author_id, published_at FROM videos WHERE author_id = f.followee_id AND status = 'PUBLISHED' ORDER BY published_at DESC, id DESC LIMIT 21) v ON true LEFT JOIN users u ON u.id = v.author_id WHERE f.follower_id = 0 ORDER BY v.published_at DESC, v.id DESC LIMIT 21;
DELETE FROM videos WHERE author_id <= 10;
ANALYZE videos;
\echo 'CASE 10 follows with no published videos / first page'
EXPLAIN (ANALYZE, BUFFERS) SELECT videos.id, users.username FROM videos LEFT JOIN users ON users.id = videos.author_id WHERE videos.status = 'PUBLISHED' AND EXISTS (SELECT 1 FROM follows f0 WHERE f0.follower_id = 0) AND EXISTS (SELECT 1 FROM follows WHERE follows.follower_id = 0 AND follows.followee_id = videos.author_id) ORDER BY videos.published_at DESC, videos.id DESC LIMIT 21;
\echo 'LATERAL comparison'
EXPLAIN (ANALYZE, BUFFERS) SELECT v.id, u.username FROM follows f JOIN LATERAL (SELECT id, author_id, published_at FROM videos WHERE author_id = f.followee_id AND status = 'PUBLISHED' ORDER BY published_at DESC, id DESC LIMIT 21) v ON true LEFT JOIN users u ON u.id = v.author_id WHERE f.follower_id = 0 ORDER BY v.published_at DESC, v.id DESC LIMIT 21;
DELETE FROM follows;
INSERT INTO follows SELECT 0, i FROM generate_series(991,1000) i;
ANALYZE follows;
\echo 'CASE 10 follows concentrated at high author IDs / first page'
EXPLAIN (ANALYZE, BUFFERS) SELECT videos.id, users.username FROM videos LEFT JOIN users ON users.id = videos.author_id WHERE videos.status = 'PUBLISHED' AND EXISTS (SELECT 1 FROM follows f0 WHERE f0.follower_id = 0) AND EXISTS (SELECT 1 FROM follows WHERE follows.follower_id = 0 AND follows.followee_id = videos.author_id) ORDER BY videos.published_at DESC, videos.id DESC LIMIT 21;
\echo 'LATERAL comparison'
EXPLAIN (ANALYZE, BUFFERS) SELECT v.id, u.username FROM follows f JOIN LATERAL (SELECT id, author_id, published_at FROM videos WHERE author_id = f.followee_id AND status = 'PUBLISHED' ORDER BY published_at DESC, id DESC LIMIT 21) v ON true LEFT JOIN users u ON u.id = v.author_id WHERE f.follower_id = 0 ORDER BY v.published_at DESC, v.id DESC LIMIT 21;
DELETE FROM follows;
ANALYZE follows;
\echo 'CASE 0 follows / first page'
EXPLAIN (ANALYZE, BUFFERS) SELECT videos.id, users.username FROM videos LEFT JOIN users ON users.id = videos.author_id WHERE videos.status = 'PUBLISHED' AND EXISTS (SELECT 1 FROM follows f0 WHERE f0.follower_id = 0) AND EXISTS (SELECT 1 FROM follows WHERE follows.follower_id = 0 AND follows.followee_id = videos.author_id) ORDER BY videos.published_at DESC, videos.id DESC LIMIT 21;
\echo 'LATERAL comparison'
EXPLAIN (ANALYZE, BUFFERS) SELECT v.id, u.username FROM follows f JOIN LATERAL (SELECT id, author_id, published_at FROM videos WHERE author_id = f.followee_id AND status = 'PUBLISHED' ORDER BY published_at DESC, id DESC LIMIT 21) v ON true LEFT JOIN users u ON u.id = v.author_id WHERE f.follower_id = 0 ORDER BY v.published_at DESC, v.id DESC LIMIT 21;
