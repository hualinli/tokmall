-- 关注 Feed 压测造数：关注关系分档（幂等，重复执行 ON CONFLICT 跳过）。
-- 档位（按 username 排序的 rn 划分，seed 用户 id 连续可作 rn 映射）：
--   rn 1..50     → 关注 1000 人（上限档，压测用户 seed000001..000050）
--   rn 51..100   → 关注 100 人（seed000051..000100）
--   rn 101..150  → 关注 10 人（seed000101..000150）
--   rn 151..200  → 关注 0 人（空短路场景，seed000151..000200）
-- followee 取 rn 偏移段，天然避开自关注（CHECK 约束），且每个 follower 关注
-- 的都是有视频的 seed 作者（前 1000 个作者各 ~33 条 PUBLISHED 视频，> K=5）。
-- 用法: docker exec -i tokmall-postgres-e2e psql -U tokmall -d tokmall < scripts/seed_follows.sql

-- 关注 1000 档：follower rn 1..50，关注 rn+1 .. rn+1000（5 万条）
INSERT INTO follows (follower_id, followee_id)
SELECT f.id, t.id
FROM (
    SELECT id, row_number() OVER (ORDER BY id) AS rn FROM users WHERE username LIKE 'seed%'
) f
JOIN (
    SELECT id, row_number() OVER (ORDER BY id) AS rn FROM users WHERE username LIKE 'seed%'
) t ON t.rn BETWEEN f.rn + 1 AND f.rn + 1000
WHERE f.rn <= 50
ON CONFLICT DO NOTHING;

-- 关注 100 档：follower rn 51..100，关注 rn+1 .. rn+100（5 千条）
INSERT INTO follows (follower_id, followee_id)
SELECT f.id, t.id
FROM (
    SELECT id, row_number() OVER (ORDER BY id) AS rn FROM users WHERE username LIKE 'seed%'
) f
JOIN (
    SELECT id, row_number() OVER (ORDER BY id) AS rn FROM users WHERE username LIKE 'seed%'
) t ON t.rn BETWEEN f.rn + 1 AND f.rn + 100
WHERE f.rn BETWEEN 51 AND 100
ON CONFLICT DO NOTHING;

-- 关注 10 档：follower rn 101..150，关注 rn+1 .. rn+10（5 百条）
INSERT INTO follows (follower_id, followee_id)
SELECT f.id, t.id
FROM (
    SELECT id, row_number() OVER (ORDER BY id) AS rn FROM users WHERE username LIKE 'seed%'
) f
JOIN (
    SELECT id, row_number() OVER (ORDER BY id) AS rn FROM users WHERE username LIKE 'seed%'
) t ON t.rn BETWEEN f.rn + 1 AND f.rn + 10
WHERE f.rn BETWEEN 101 AND 150
ON CONFLICT DO NOTHING;

ANALYZE follows;

-- 验证档位统计：每档 follower 数、人均关注数
SELECT 'follow1000=' || count(*) FROM follows f
  JOIN users u ON u.id = f.follower_id WHERE u.username LIKE 'seed0000%' AND u.username <= 'seed000050';
SELECT 'follow100=' || count(*) FROM follows f
  JOIN users u ON u.id = f.follower_id WHERE u.username >= 'seed000051' AND u.username <= 'seed000100';
SELECT 'follow10=' || count(*) FROM follows f
  JOIN users u ON u.id = f.follower_id WHERE u.username >= 'seed000101' AND u.username <= 'seed000150';
SELECT 'total_follows=' || count(*) FROM follows;
