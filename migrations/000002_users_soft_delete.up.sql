-- 软删除：逻辑删除标记，GORM DeletedAt 自动维护
ALTER TABLE users ADD COLUMN deleted_at TIMESTAMPTZ;

-- 业务枚举约束移到应用层校验，避免新增角色时反复改库
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users DROP CONSTRAINT users_status_check;

CREATE INDEX idx_users_deleted_at ON users (deleted_at);
