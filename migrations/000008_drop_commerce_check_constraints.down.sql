-- 回滚：恢复 Commerce 表的 CHECK 约束（如果需要）。

ALTER TABLE products ADD CONSTRAINT products_status_check
    CHECK (status IN ('ON_SALE', 'OFF_SALE', 'DELETED'));

ALTER TABLE skus ADD CONSTRAINT skus_status_check
    CHECK (status IN ('ACTIVE', 'DISABLED'));

ALTER TABLE inventory ADD CONSTRAINT inventory_quantity_check
    CHECK (quantity >= 0);
