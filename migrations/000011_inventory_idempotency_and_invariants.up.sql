-- Commerce 地基收口：恢复核心数据库不变量，并把库存流水幂等范围
-- 从全局 biz_id 收窄为 (sku_id, reason, biz_id)。
--
-- 这是一份前向迁移，不修改 007~010 历史文件。约束 VALIDATE 失败时应
-- 让迁移失败并暴露脏数据，而不是静默修正交易事实。

-- 旧版本允许 SKU 先存在、inventory 行按首次调整惰性创建。为保证新
-- 不变量，对已经存在但缺行的 SKU 补零库存；以后由 product.CreateSKU
-- 在同一事务中创建 SKU 和 inventory 行。
INSERT INTO inventory (sku_id, quantity, version, updated_at)
SELECT s.id, 0, 0, now()
FROM skus s
LEFT JOIN inventory i ON i.sku_id = s.id
WHERE i.sku_id IS NULL;

ALTER TABLE inventory_transactions
    DROP CONSTRAINT IF EXISTS inventory_transactions_biz_id_key;

ALTER TABLE inventory_transactions
    ADD CONSTRAINT inventory_transactions_sku_reason_biz_id_key
    UNIQUE (sku_id, reason, biz_id);

ALTER TABLE products
    ADD CONSTRAINT products_merchant_id_positive_check
    CHECK (merchant_id > 0);

ALTER TABLE products
    ADD CONSTRAINT products_title_nonempty_check
    CHECK (btrim(title) <> '');

ALTER TABLE products
    ADD CONSTRAINT products_status_check
    CHECK (status IN ('ON_SALE', 'OFF_SALE', 'DELETED'));

ALTER TABLE skus
    ADD CONSTRAINT skus_price_positive_check
    CHECK (price > 0);

ALTER TABLE skus
    ADD CONSTRAINT skus_status_check
    CHECK (status IN ('ACTIVE', 'DISABLED'));

ALTER TABLE inventory
    ADD CONSTRAINT inventory_quantity_check
    CHECK (quantity >= 0);

ALTER TABLE inventory_transactions
    ADD CONSTRAINT inventory_transactions_change_nonzero_check
    CHECK (change <> 0);

ALTER TABLE inventory_transactions
    ADD CONSTRAINT inventory_transactions_reason_check
    CHECK (reason IN ('ADJUST', 'ORDER_CREATED', 'ORDER_CANCELED', 'ORDER_EXPIRED'));
