-- 售后（退款）闭环：订单主状态新增 REFUND_REQUESTED / REFUNDED，
-- 新增退款单 refunds 记录售后文档。
--
-- 设计取舍：退款是"申请 → 商家裁决 → 执行"三段式，天然存在两个状态维度
-- （订单履约维度 / 售后单据维度）。这里把售后状态投影进订单主状态，
-- 好处是"发货只能从 PAID 出发"这类守卫继续由订单状态机承担，
-- 不需要在每条履约命令里额外查一次退款表，也就没有"查完再写"的竞态窗口。
-- 订单进入 REFUND_REQUESTED 时用 refund_from_status 记住裁决被拒后要退回的状态。

ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (status IN (
    'PENDING_PAYMENT',
    'PAID',
    'FULFILLING',
    'COMPLETED',
    'CANCELED',
    'EXPIRED',
    'REFUND_REQUESTED',
    'REFUNDED'
));

ALTER TABLE orders
    ADD COLUMN refund_from_status TEXT,
    ADD COLUMN refunded_at TIMESTAMPTZ;

ALTER TABLE orders DROP CONSTRAINT orders_state_timestamps_check;
ALTER TABLE orders ADD CONSTRAINT orders_state_timestamps_check CHECK (
    (status IN ('PAID', 'FULFILLING', 'COMPLETED', 'REFUND_REQUESTED', 'REFUNDED'))
        = (paid_at IS NOT NULL)
    AND (status = 'CANCELED') = (canceled_at IS NOT NULL)
    AND (status = 'EXPIRED') = (expired_at IS NOT NULL)
);

-- 履约时间戳是历史事实：订单发货后退款，fulfilling_at 不能被清空。
-- 因此 014 的"状态与时间戳同真同假"改为"时间戳存在则状态必须已经历过该阶段"。
ALTER TABLE orders DROP CONSTRAINT orders_fulfillment_timestamps_check;
ALTER TABLE orders ADD CONSTRAINT orders_fulfillment_timestamps_check CHECK (
    (status = 'COMPLETED') = (completed_at IS NOT NULL)
    AND (
        (status IN ('REFUND_REQUESTED', 'REFUNDED')
            AND (refund_from_status = 'FULFILLING') = (fulfilling_at IS NOT NULL))
        OR
        (status NOT IN ('REFUND_REQUESTED', 'REFUNDED')
            AND (status IN ('FULFILLING', 'COMPLETED')) = (fulfilling_at IS NOT NULL))
    )
);

ALTER TABLE orders ADD CONSTRAINT orders_refund_columns_check CHECK (
    (status IN ('REFUND_REQUESTED', 'REFUNDED')) = (refund_from_status IS NOT NULL)
    AND (status = 'REFUNDED') = (refunded_at IS NOT NULL)
    AND (refund_from_status IS NULL OR refund_from_status IN ('PAID', 'FULFILLING'))
);

-- 商家售后工作台按"待裁决"过滤，是最高频的售后查询。
CREATE INDEX idx_orders_merchant_refund_pending
    ON orders (merchant_id, id DESC)
    WHERE status = 'REFUND_REQUESTED';

CREATE TABLE refunds (
    id              BIGSERIAL PRIMARY KEY,
    refund_no       TEXT NOT NULL
                    CONSTRAINT refunds_refund_no_check
                    CHECK (btrim(refund_no) <> '' AND length(refund_no) <= 64),
    order_id        BIGINT NOT NULL REFERENCES orders(id),
    user_id         BIGINT NOT NULL REFERENCES users(id),
    merchant_id     BIGINT NOT NULL
                    CONSTRAINT refunds_merchant_id_positive_check
                    CHECK (merchant_id > 0),
    amount          BIGINT NOT NULL
                    CONSTRAINT refunds_amount_positive_check
                    CHECK (amount > 0),
    reason          TEXT NOT NULL
                    CONSTRAINT refunds_reason_check
                    CHECK (btrim(reason) <> '' AND length(reason) <= 200),
    status          TEXT NOT NULL DEFAULT 'REQUESTED'
                    CONSTRAINT refunds_status_check
                    CHECK (status IN ('REQUESTED', 'REJECTED', 'SUCCEEDED')),
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ,
    decided_by      BIGINT,
    decision_remark TEXT,
    refunded_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT refunds_refund_no_key UNIQUE (refund_no),
    CONSTRAINT refunds_decision_check CHECK (
        (status = 'REQUESTED') = (decided_at IS NULL)
        AND (status = 'REQUESTED') = (decided_by IS NULL)
        AND (status = 'SUCCEEDED') = (refunded_at IS NOT NULL)
        AND (decision_remark IS NULL OR btrim(decision_remark) <> '')
    )
);

-- 同一订单最多存在一笔"未决或已成功"的退款：并发重复申请由该唯一索引仲裁，
-- 而不是靠应用层先查后写。被拒绝后允许重新申请，因此必须用部分索引。
CREATE UNIQUE INDEX idx_refunds_order_active
    ON refunds (order_id)
    WHERE status IN ('REQUESTED', 'SUCCEEDED');

CREATE INDEX idx_refunds_merchant_status ON refunds (merchant_id, status, id DESC);
CREATE INDEX idx_refunds_user_id_desc ON refunds (user_id, id DESC);

-- 库存流水新增退款回补原因。回补只在"未发货退款"时发生，
-- 且与( sku_id, reason, biz_id )唯一约束配合保证只回补一次。
ALTER TABLE inventory_transactions DROP CONSTRAINT inventory_transactions_reason_check;
ALTER TABLE inventory_transactions ADD CONSTRAINT inventory_transactions_reason_check
    CHECK (reason IN ('ADJUST', 'ORDER_CREATED', 'ORDER_CANCELED', 'ORDER_EXPIRED', 'ORDER_REFUNDED'));
