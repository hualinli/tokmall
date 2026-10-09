-- 若已有退券后再次使用的历史，旧约束无法恢复；拒绝回滚，不删历史数据。
DO $$ BEGIN
    IF EXISTS (SELECT user_coupon_id FROM orders WHERE user_coupon_id IS NOT NULL
               GROUP BY user_coupon_id HAVING count(*) > 1) THEN
        RAISE EXCEPTION 'cannot restore coupon index after coupon reuse';
    END IF;
END $$;
DROP INDEX idx_orders_user_coupon_unique;
CREATE UNIQUE INDEX idx_orders_user_coupon_unique ON orders (user_coupon_id)
    WHERE user_coupon_id IS NOT NULL;
