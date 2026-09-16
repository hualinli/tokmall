-- Commerce v1 订单地基。金额统一使用最小货币单位（当前为人民币分），
-- 不使用浮点数。订单创建、扣库存和库存流水将在后续迁移之上的同一事务完成。

CREATE TABLE video_products (
    id          BIGSERIAL PRIMARY KEY,
    video_id    BIGINT NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    author_id   BIGINT NOT NULL REFERENCES users(id),
    product_id  BIGINT NOT NULL REFERENCES products(id),
    status      TEXT NOT NULL DEFAULT 'ACTIVE'
                CONSTRAINT video_products_status_check
                CHECK (status IN ('ACTIVE', 'DISABLED')),
    sort_order  INTEGER NOT NULL DEFAULT 0
                CONSTRAINT video_products_sort_order_nonnegative_check
                CHECK (sort_order >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT video_products_video_product_key UNIQUE (video_id, product_id)
);

-- Feed 按一页 video_id 批量装配启用的商品卡。
CREATE INDEX idx_video_products_active_video_sort
    ON video_products (video_id, sort_order, id)
    WHERE status = 'ACTIVE';

-- 作者维护自己的挂载关系，以及后续按作者排查归因来源。
CREATE INDEX idx_video_products_author_status
    ON video_products (author_id, status, id DESC);

CREATE TABLE orders (
    id                  BIGSERIAL PRIMARY KEY,
    order_no            TEXT NOT NULL
                        CONSTRAINT orders_order_no_nonempty_check
                        CHECK (btrim(order_no) <> '' AND length(order_no) <= 64),
    user_id             BIGINT NOT NULL REFERENCES users(id),
    merchant_id         BIGINT NOT NULL
                        CONSTRAINT orders_merchant_id_positive_check
                        CHECK (merchant_id > 0),
    status              TEXT NOT NULL DEFAULT 'PENDING_PAYMENT'
                        CONSTRAINT orders_status_check
                        CHECK (status IN (
                            'PENDING_PAYMENT',
                            'PAID',
                            'FULFILLING',
                            'COMPLETED',
                            'CANCELED',
                            'EXPIRED'
                        )),
    total_amount        BIGINT NOT NULL
                        CONSTRAINT orders_total_amount_positive_check
                        CHECK (total_amount > 0),
    idempotency_key     TEXT NOT NULL
                        CONSTRAINT orders_idempotency_key_check
                        CHECK (btrim(idempotency_key) <> '' AND length(idempotency_key) <= 128),
    request_fingerprint TEXT NOT NULL
                        CONSTRAINT orders_request_fingerprint_check
                        CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),

    -- 以下是下单时的归因事实快照，刻意不设外键：作者软删、视频硬删、
    -- 商品下架或未来归档，都不能破坏历史订单与统计口径。
    source_video_id     BIGINT,
    source_author_id    BIGINT,
    source_product_id   BIGINT,

    expires_at          TIMESTAMPTZ NOT NULL,
    paid_at             TIMESTAMPTZ,
    canceled_at         TIMESTAMPTZ,
    expired_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT orders_order_no_key UNIQUE (order_no),
    CONSTRAINT orders_user_idempotency_key UNIQUE (user_id, idempotency_key),
    CONSTRAINT orders_source_all_or_none_check CHECK (
        (source_video_id IS NULL AND source_author_id IS NULL AND source_product_id IS NULL)
        OR
        (source_video_id IS NOT NULL AND source_author_id IS NOT NULL AND source_product_id IS NOT NULL)
    ),
    CONSTRAINT orders_state_timestamps_check CHECK (
        (status IN ('PAID', 'FULFILLING', 'COMPLETED')) = (paid_at IS NOT NULL)
        AND (status = 'CANCELED') = (canceled_at IS NOT NULL)
        AND (status = 'EXPIRED') = (expired_at IS NOT NULL)
    ),
    CONSTRAINT orders_expiry_after_creation_check CHECK (expires_at > created_at)
);

-- 用户订单列表按状态过滤并稳定倒序分页。
CREATE INDEX idx_orders_user_status_created
    ON orders (user_id, status, created_at DESC, id DESC);

-- 多实例超时关单 Worker 扫描到期的待支付订单；后续配合
-- FOR UPDATE SKIP LOCKED 使用。
CREATE INDEX idx_orders_pending_expiry
    ON orders (expires_at, id)
    WHERE status = 'PENDING_PAYMENT';

-- 成交后按来源作者/视频/商品聚合，NULL 来源不进入索引。
CREATE INDEX idx_orders_source_author_paid
    ON orders (source_author_id, paid_at DESC, id DESC)
    WHERE status IN ('PAID', 'FULFILLING', 'COMPLETED')
      AND source_author_id IS NOT NULL;

CREATE INDEX idx_orders_source_video_paid
    ON orders (source_video_id, paid_at DESC, id DESC)
    WHERE status IN ('PAID', 'FULFILLING', 'COMPLETED')
      AND source_video_id IS NOT NULL;

CREATE TABLE order_items (
    id                     BIGSERIAL PRIMARY KEY,
    order_id               BIGINT NOT NULL REFERENCES orders(id),

    -- ID 用于对账和追踪；快照字段才是历史订单展示与金额计算依据。
    product_id             BIGINT NOT NULL REFERENCES products(id),
    sku_id                 BIGINT NOT NULL REFERENCES skus(id),
    merchant_id_snapshot   BIGINT NOT NULL
                           CONSTRAINT order_items_merchant_positive_check
                           CHECK (merchant_id_snapshot > 0),
    product_title_snapshot TEXT NOT NULL
                           CONSTRAINT order_items_product_title_nonempty_check
                           CHECK (btrim(product_title_snapshot) <> ''),
    sku_spec_snapshot      TEXT NOT NULL DEFAULT '',
    unit_price             BIGINT NOT NULL
                           CONSTRAINT order_items_unit_price_positive_check
                           CHECK (unit_price > 0),
    quantity               INTEGER NOT NULL
                           CONSTRAINT order_items_quantity_positive_check
                           CHECK (quantity > 0),
    line_amount            BIGINT NOT NULL
                           CONSTRAINT order_items_line_amount_positive_check
                           CHECK (line_amount > 0),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Commerce v1 明确只支持单 SKU 订单。未来支持购物车/拆单时通过新迁移
    -- 删除该唯一约束，而不是让当前 service 约定成为数据库外的隐含规则。
    CONSTRAINT order_items_single_item_per_order_key UNIQUE (order_id)
);

CREATE INDEX idx_order_items_sku ON order_items (sku_id, order_id);

CREATE TABLE payments (
    id                BIGSERIAL PRIMARY KEY,
    payment_no        TEXT NOT NULL
                      CONSTRAINT payments_payment_no_nonempty_check
                      CHECK (btrim(payment_no) <> '' AND length(payment_no) <= 64),
    order_id          BIGINT NOT NULL REFERENCES orders(id),
    amount            BIGINT NOT NULL
                      CONSTRAINT payments_amount_positive_check
                      CHECK (amount > 0),
    status            TEXT NOT NULL DEFAULT 'PENDING'
                      CONSTRAINT payments_status_check
                      CHECK (status IN ('PENDING', 'SUCCESS', 'FAILED')),
    provider_trade_no TEXT,
    failure_code      TEXT,
    paid_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payments_payment_no_key UNIQUE (payment_no),
    CONSTRAINT payments_provider_trade_no_nonempty_check CHECK (
        provider_trade_no IS NULL OR btrim(provider_trade_no) <> ''
    ),
    CONSTRAINT payments_success_paid_at_check CHECK (
        (status = 'SUCCESS' AND paid_at IS NOT NULL)
        OR
        (status <> 'SUCCESS' AND paid_at IS NULL)
    )
);

-- 一笔订单允许多次支付尝试；不能对 order_id 加唯一约束。
CREATE INDEX idx_payments_order_created
    ON payments (order_id, created_at DESC, id DESC);

CREATE UNIQUE INDEX idx_payments_provider_trade_no_unique
    ON payments (provider_trade_no)
    WHERE provider_trade_no IS NOT NULL;
