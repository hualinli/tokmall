-- 回滚不变量加固。邮箱大小写唯一索引可以安全删除（数据仍保持小写，
-- 只是失去"未来写入"的兜底）；published_at 的 CHECK 也可以删除。
-- 这两项都不改变既有数据，因此不设拒绝条件。
DROP INDEX IF EXISTS uq_users_email_lower;

ALTER TABLE videos DROP CONSTRAINT IF EXISTS videos_published_at_required_check;
