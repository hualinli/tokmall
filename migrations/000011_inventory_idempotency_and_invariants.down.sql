-- 回滚 000011 的 schema 变更。数据行不在 down 中删除，避免把迁移后
-- 合法创建的库存事实误删；在无冲突数据的情况下，唯一约束可恢复为
-- 000007 的全局 biz_id 语义。

ALTER TABLE inventory_transactions
    DROP CONSTRAINT IF EXISTS inventory_transactions_reason_check;

ALTER TABLE inventory_transactions
    DROP CONSTRAINT IF EXISTS inventory_transactions_change_nonzero_check;

ALTER TABLE inventory
    DROP CONSTRAINT IF EXISTS inventory_quantity_check;

ALTER TABLE skus
    DROP CONSTRAINT IF EXISTS skus_status_check;

ALTER TABLE skus
    DROP CONSTRAINT IF EXISTS skus_price_positive_check;

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_status_check;

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_title_nonempty_check;

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_merchant_id_positive_check;

ALTER TABLE inventory_transactions
    DROP CONSTRAINT IF EXISTS inventory_transactions_sku_reason_biz_id_key;

ALTER TABLE inventory_transactions
    ADD CONSTRAINT inventory_transactions_biz_id_key UNIQUE (biz_id);
