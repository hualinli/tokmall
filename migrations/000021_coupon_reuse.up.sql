-- 保留退款订单的历史用券引用，只约束尚未退款的占用。
DROP INDEX idx_orders_user_coupon_unique;
CREATE UNIQUE INDEX idx_orders_user_coupon_unique ON orders (user_coupon_id)
    WHERE user_coupon_id IS NOT NULL AND status <> 'REFUNDED';
