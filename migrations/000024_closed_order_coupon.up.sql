DROP INDEX idx_orders_user_coupon_unique;
CREATE UNIQUE INDEX idx_orders_user_coupon_unique ON orders(user_coupon_id)
 WHERE user_coupon_id IS NOT NULL AND status NOT IN ('REFUNDED','CANCELED','EXPIRED');

-- Repair coupons retained by historical unpaid closed orders.
UPDATE user_coupons uc SET status = CASE WHEN c.valid_to > now() THEN 'UNUSED' ELSE 'EXPIRED' END,
 used_at = NULL, order_id = NULL, updated_at = now()
FROM coupons c, orders o
WHERE uc.coupon_id = c.id AND uc.order_id = o.id AND uc.status = 'USED'
 AND o.user_coupon_id = uc.id AND o.status IN ('CANCELED','EXPIRED');
