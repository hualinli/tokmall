-- 去掉 Commerce 表中不必要的 CHECK 约束：
-- 状态枚举和库存非负等规则移到业务层校验，避免以后调整枚举/规则时频繁改库。
-- 保留：主键、唯一约束、外键、NOT NULL。

ALTER TABLE products DROP CONSTRAINT IF EXISTS products_status_check;
ALTER TABLE skus DROP CONSTRAINT IF EXISTS skus_status_check;
ALTER TABLE inventory DROP CONSTRAINT IF EXISTS inventory_quantity_check;
