ALTER TABLE orders
    ADD COLUMN fulfilling_at TIMESTAMPTZ,
    ADD COLUMN completed_at TIMESTAMPTZ;

ALTER TABLE orders ADD CONSTRAINT orders_fulfillment_timestamps_check CHECK (
    (status IN ('FULFILLING', 'COMPLETED')) = (fulfilling_at IS NOT NULL)
    AND (status = 'COMPLETED') = (completed_at IS NOT NULL)
);

CREATE INDEX idx_orders_merchant_id_desc ON orders (merchant_id, id DESC);
CREATE INDEX idx_orders_user_id_desc ON orders (user_id, id DESC);
