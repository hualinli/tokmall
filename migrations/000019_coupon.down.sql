-- 回滚优惠券域。存在已发放或已使用的券时拒绝回滚：
-- 那些记录代表已经发生的促销事实，静默删除会让订单的优惠金额失去依据。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM user_coupons) THEN
        RAISE EXCEPTION 'user_coupons is not empty; refusing to roll back 000019';
    END IF;
    IF EXISTS (SELECT 1 FROM orders WHERE user_coupon_id IS NOT NULL OR discount_amount <> 0) THEN
        RAISE EXCEPTION 'orders reference coupons; refusing to roll back 000019';
    END IF;
END $$;

DROP INDEX IF EXISTS idx_orders_user_coupon_unique;

ALTER TABLE orders DROP CONSTRAINT orders_amount_split_check;
ALTER TABLE orders DROP COLUMN user_coupon_id;
ALTER TABLE orders DROP COLUMN discount_amount;
ALTER TABLE orders DROP COLUMN original_amount;

DROP TABLE user_coupons;
DROP TABLE coupons;
