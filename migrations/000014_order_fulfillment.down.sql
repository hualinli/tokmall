DROP INDEX IF EXISTS idx_orders_user_id_desc;
DROP INDEX IF EXISTS idx_orders_merchant_id_desc;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_fulfillment_timestamps_check;
ALTER TABLE orders DROP COLUMN IF EXISTS completed_at, DROP COLUMN IF EXISTS fulfilling_at;
