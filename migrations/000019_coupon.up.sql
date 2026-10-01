-- 优惠券/促销域（v1：满减券）。
--
-- 设计要点（对照 DomTok 的 8 处缺陷逐条修正，见 docs/domtok-comparison-and-borrowing.md）：
--  1. 有发行量：total_quantity + issued_quantity，且 issued <= total 由 CHECK 兜底；
--  2. 有每人限领：per_user_limit，领取时在同一把行锁内计数（不会超领）；
--  3. 有领取截止：valid_from/valid_to 在领取与使用时都校验（DomTok 只在下单算价时看一眼）；
--  4. 券有状态机：user_coupons.status ∈ UNUSED/USED/EXPIRED，核销是条件更新而不是死代码；
--  5. 金额用整数分（BIGINT），不用 float64；
--  6. 门槛按**整单商品金额**比较，不按单行；
--  7. 退款退券：全额退款成功且券仍在有效期内 → 退回 UNUSED；
--  8. 对账覆盖：发行量守恒、券面额与订单优惠一致（见 internal/reconcile）。

CREATE TABLE coupons (
    id                BIGSERIAL PRIMARY KEY,
    -- 谁发的券。本项目 v1 只有商家发券（平台券/类目券是后续演进）。
    merchant_id       BIGINT NOT NULL REFERENCES users(id),
    title             TEXT NOT NULL
                      CONSTRAINT coupons_title_check
                      CHECK (btrim(title) <> '' AND length(title) <= 100),
    -- 满减：订单商品金额 >= threshold_amount 时可减 discount_amount。
    -- 两个约束保证"减不超过满"，因此实付金额恒 >= 0。
    threshold_amount  BIGINT NOT NULL
                      CONSTRAINT coupons_threshold_positive_check CHECK (threshold_amount > 0),
    discount_amount   BIGINT NOT NULL
                      CONSTRAINT coupons_discount_positive_check CHECK (discount_amount > 0),
    CONSTRAINT coupons_discount_not_exceed_threshold_check CHECK (discount_amount <= threshold_amount),

    -- 发行量：0 表示不限量。issued_quantity 只在领取事务里 +1，
    -- 且由 CHECK 保证不会超过总量（应用层的条件更新是第一道防线，这里是第二道）。
    total_quantity    BIGINT NOT NULL DEFAULT 0
                      CONSTRAINT coupons_total_quantity_nonnegative_check CHECK (total_quantity >= 0),
    issued_quantity   BIGINT NOT NULL DEFAULT 0
                      CONSTRAINT coupons_issued_quantity_nonnegative_check CHECK (issued_quantity >= 0),
    CONSTRAINT coupons_issued_not_exceed_total_check
        CHECK (total_quantity = 0 OR issued_quantity <= total_quantity),

    per_user_limit    INTEGER NOT NULL DEFAULT 1
                      CONSTRAINT coupons_per_user_limit_positive_check CHECK (per_user_limit > 0),

    status            TEXT NOT NULL DEFAULT 'ACTIVE'
                      CONSTRAINT coupons_status_check CHECK (status IN ('ACTIVE', 'DISABLED')),

    valid_from        TIMESTAMPTZ NOT NULL,
    valid_to          TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT coupons_validity_window_check CHECK (valid_to > valid_from)
);

-- 用户侧"可领取的券"列表：按有效期倒序 + 只取 ACTIVE。
CREATE INDEX idx_coupons_available ON coupons (valid_to DESC, id DESC) WHERE status = 'ACTIVE';
-- 商家券管理列表。
CREATE INDEX idx_coupons_merchant ON coupons (merchant_id, id DESC);

CREATE TABLE user_coupons (
    id          BIGSERIAL PRIMARY KEY,
    coupon_id   BIGINT NOT NULL REFERENCES coupons(id),
    user_id     BIGINT NOT NULL REFERENCES users(id),
    status      TEXT NOT NULL DEFAULT 'UNUSED'
                CONSTRAINT user_coupons_status_check CHECK (status IN ('UNUSED', 'USED', 'EXPIRED')),
    claimed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    used_at     TIMESTAMPTZ,
    -- 用在哪张订单：核销时写入，退款退券时清空。它是"券与订单"的唯一关联，
    -- 也是退款/对账定位"这张券用在哪"的依据。
    order_id    BIGINT REFERENCES orders(id),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- 状态与时间戳/订单三者必须同真同假，避免出现"已使用但没有订单"这种无法对账的状态。
    CONSTRAINT user_coupons_usage_check CHECK (
        (status = 'USED') = (used_at IS NOT NULL)
        AND (status = 'USED') = (order_id IS NOT NULL)
    )
);

-- 我的券列表。
CREATE INDEX idx_user_coupons_user_status ON user_coupons (user_id, status, id DESC);
-- 发行量对账与"每人限领"计数：都按 (coupon_id, user_id) 聚合。
CREATE INDEX idx_user_coupons_coupon_user ON user_coupons (coupon_id, user_id);

-- 订单增加金额拆分与用券引用。
--   original_amount 原价（商品金额合计）
--   discount_amount 优惠金额（券面额，v1 单 SKU 订单不会超过原价）
--   total_amount    实付金额 = original_amount - discount_amount（保持既有语义：支付金额）
-- GMV 口径（ADR-017 的延伸）：GMV 记**实付**，优惠单列，退款按发生日单独归集。
ALTER TABLE orders
    ADD COLUMN original_amount BIGINT,
    ADD COLUMN discount_amount BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN user_coupon_id  BIGINT REFERENCES user_coupons(id);

-- 历史订单没有原价概念：回填为实付金额、优惠为 0，然后收紧为 NOT NULL。
UPDATE orders SET original_amount = total_amount WHERE original_amount IS NULL;

ALTER TABLE orders
    ALTER COLUMN original_amount SET NOT NULL,
    ADD CONSTRAINT orders_amount_split_check CHECK (
        discount_amount >= 0
        AND original_amount > 0
        AND total_amount > 0
        AND total_amount = original_amount - discount_amount
    );

-- 一张券最多核销到一张订单上（退款退券后会清空 order_id，因此用部分唯一索引）。
CREATE UNIQUE INDEX idx_orders_user_coupon_unique
    ON orders (user_coupon_id) WHERE user_coupon_id IS NOT NULL;
