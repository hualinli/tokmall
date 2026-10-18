DROP INDEX IF EXISTS idx_orders_auto_complete_due;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_auto_complete_check;
ALTER TABLE orders DROP COLUMN IF EXISTS auto_complete_at;
