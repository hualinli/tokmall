-- 回滚退款闭环。只在没有退款数据时可用；存在退款单时拒绝回滚，
-- 避免把已发生的资金事实静默丢弃。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM refunds) THEN
        RAISE EXCEPTION 'refunds table is not empty; refusing to roll back 000015';
    END IF;
    IF EXISTS (SELECT 1 FROM orders WHERE status IN ('REFUND_REQUESTED', 'REFUNDED')) THEN
        RAISE EXCEPTION 'orders still reference refund states; refusing to roll back 000015';
    END IF;
END $$;

ALTER TABLE inventory_transactions DROP CONSTRAINT inventory_transactions_reason_check;
ALTER TABLE inventory_transactions ADD CONSTRAINT inventory_transactions_reason_check
    CHECK (reason IN ('ADJUST', 'ORDER_CREATED', 'ORDER_CANCELED', 'ORDER_EXPIRED'));

DROP TABLE refunds;

DROP INDEX IF EXISTS idx_orders_merchant_refund_pending;

ALTER TABLE orders DROP CONSTRAINT orders_refund_columns_check;
ALTER TABLE orders DROP CONSTRAINT orders_fulfillment_timestamps_check;
ALTER TABLE orders ADD CONSTRAINT orders_fulfillment_timestamps_check CHECK (
    (status IN ('FULFILLING', 'COMPLETED')) = (fulfilling_at IS NOT NULL)
    AND (status = 'COMPLETED') = (completed_at IS NOT NULL)
);
ALTER TABLE orders DROP CONSTRAINT orders_state_timestamps_check;
ALTER TABLE orders ADD CONSTRAINT orders_state_timestamps_check CHECK (
    (status IN ('PAID', 'FULFILLING', 'COMPLETED')) = (paid_at IS NOT NULL)
    AND (status = 'CANCELED') = (canceled_at IS NOT NULL)
    AND (status = 'EXPIRED') = (expired_at IS NOT NULL)
);

ALTER TABLE orders DROP COLUMN refunded_at;
ALTER TABLE orders DROP COLUMN refund_from_status;

ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (status IN (
    'PENDING_PAYMENT', 'PAID', 'FULFILLING', 'COMPLETED', 'CANCELED', 'EXPIRED'
));
