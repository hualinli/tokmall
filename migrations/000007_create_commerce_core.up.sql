CREATE TABLE products (
    id          BIGSERIAL PRIMARY KEY,
    merchant_id BIGINT NOT NULL,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    cover_url   TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'ON_SALE'
                CHECK (status IN ('ON_SALE', 'OFF_SALE', 'DELETED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_products_merchant_status
    ON products (merchant_id, status, id);

CREATE TABLE skus (
    id          BIGSERIAL PRIMARY KEY,
    product_id  BIGINT NOT NULL REFERENCES products(id),
    spec        TEXT NOT NULL DEFAULT '',
    price       BIGINT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'ACTIVE'
                CHECK (status IN ('ACTIVE', 'DISABLED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_skus_product
    ON skus (product_id, status, id);

CREATE TABLE inventory (
    sku_id      BIGINT PRIMARY KEY REFERENCES skus(id),
    quantity    BIGINT NOT NULL CHECK (quantity >= 0),
    version     BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE inventory_transactions (
    id          BIGSERIAL PRIMARY KEY,
    sku_id      BIGINT NOT NULL REFERENCES skus(id),
    order_id    BIGINT,
    change      BIGINT NOT NULL,
    reason      TEXT NOT NULL,
    biz_id      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (biz_id)
);
