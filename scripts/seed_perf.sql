-- 性能压测造数：30k 用户 + 1M 已发布视频（published_at 秒级铺开，模拟 11.6 天增量）。
-- 幂等：用户按唯一键 ON CONFLICT；视频按 title 前缀去重（重复执行直接跳过）。
-- 用法: docker exec -i tokmall-postgres-e2e psql -U tokmall -d tokmall < scripts/seed_perf.sql

INSERT INTO users (email, username, password_hash, bio, role, status, created_at, updated_at)
SELECT 'u' || i || '@seed.local',
       'seed' || lpad(i::text, 6, '0'),
       '$2a$10$bSPKSKJUdh257leaWepIZeXFZXWgmWxvz6UaPXUFqWCzc28m6boDm',
       '',
       'USER',
       'ACTIVE',
       now(),
       now()
FROM generate_series(1, 30000) i
ON CONFLICT (email) DO NOTHING;

INSERT INTO videos (author_id, title, object_key, duration_seconds, status, published_at, created_at, updated_at)
SELECT su.id,
       'seed video ' || i,
       'videos/seed/' || i || '.mp4',
       30,
       'PUBLISHED',
       now() - (i || ' seconds')::interval,
       now(),
       now()
FROM generate_series(1, 1000000) i
JOIN (
    -- 种子用户的真实 id 未必从 1 开始（序列可能接续旧数据），
    -- 用 row_number 把 1..30000 映射到真实 id。
    SELECT id, row_number() OVER (ORDER BY id) AS rn
    FROM users
    WHERE username LIKE 'seed%'
) su ON su.rn = ((i - 1) % 30000) + 1
WHERE (SELECT count(*) FROM videos WHERE title LIKE 'seed video %') < 1000000;

ANALYZE users;
ANALYZE videos;

SELECT 'users=' || count(*) FROM users;
SELECT 'videos=' || count(*) FROM videos;
SELECT 'published=' || count(*) FROM videos WHERE status = 'PUBLISHED';
