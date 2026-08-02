-- 回滚：撤销软删除列、恢复 CHECK 约束
ALTER TABLE users DROP COLUMN deleted_at;
DROP INDEX IF EXISTS idx_users_deleted_at;

ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('USER', 'MERCHANT', 'ADMIN'));
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('ACTIVE', 'DISABLED', 'DELETED'));
