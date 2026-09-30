-- 把两条"只存在于应用层约定"的不变量下沉到数据库：
--  1. PUBLISHED 视频必须有 published_at（feed 游标依赖它做键集分页，
--     缺值会让翻页代码解引用空指针）；
--  2. 邮箱大小写不敏感唯一（注册已统一小写存储，唯一索引是最后防线，
--     防止将来出现绕过 service 的写入路径造出 Alice@x.com / alice@x.com）。
--
-- 迁移遇到脏数据必须失败并暴露，而不是静默修正：这两条都是历史事实的
-- 一致性问题，悄悄改写会让"谁改的、什么时候改的"失去痕迹。

-- 1) 邮箱大小写重复检测（先查后改，避免 UPDATE 撞唯一约束时报错难读）
DO $$
DECLARE
    dup_count INTEGER;
BEGIN
    SELECT count(*) INTO dup_count FROM (
        SELECT lower(btrim(email)) FROM users GROUP BY 1 HAVING count(*) > 1
    ) d;
    IF dup_count > 0 THEN
        RAISE EXCEPTION 'users.email has % case-insensitive duplicates; resolve them before 000016', dup_count;
    END IF;
END $$;

UPDATE users SET email = lower(btrim(email)) WHERE email <> lower(btrim(email));

CREATE UNIQUE INDEX IF NOT EXISTS uq_users_email_lower ON users (lower(email));

-- 2) 已发布视频必须有发布时间
ALTER TABLE videos ADD CONSTRAINT videos_published_at_required_check
    CHECK (status <> 'PUBLISHED' OR published_at IS NOT NULL);
