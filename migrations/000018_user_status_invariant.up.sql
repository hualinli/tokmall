-- 修复并锁死 users.status：000002 把状态枚举约束移到了应用层，
-- 但 status 与 role 不同——空字符串会让"账号是否可用"的判断全部失败
-- （既不是 ACTIVE 也不是 DISABLED，登录/续签都会被拒），
-- 属于必须由数据库兜底的写入错误。
--
-- 背景：给 model 映射 status 字段后，GORM 的 Create 会把零值（空串）
-- 写进该列，绕过 DEFAULT 'ACTIVE'。这个问题是端到端演练时被发现的
-- （注册成功但立刻登录失败），因此这里既修数据也加约束。

UPDATE users SET status = 'ACTIVE' WHERE status IS NULL OR btrim(status) = '';

ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('ACTIVE', 'DISABLED', 'DELETED'));
