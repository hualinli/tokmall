DO $$ BEGIN
 IF EXISTS (SELECT user_coupon_id FROM orders WHERE user_coupon_id IS NOT NULL AND status <> 'REFUNDED'
            GROUP BY user_coupon_id HAVING count(*) > 1) THEN
  RAISE EXCEPTION 'cannot restore coupon index after closed-order coupon reuse';
 END IF;
END $$;
DROP INDEX idx_orders_user_coupon_unique;
CREATE UNIQUE INDEX idx_orders_user_coupon_unique ON orders(user_coupon_id)
 WHERE user_coupon_id IS NOT NULL AND status <> 'REFUNDED';
