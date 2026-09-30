-- Transactional Outbox + 幂等消费 + 作者日销投影。
--
-- 为什么需要 Outbox：支付成功之后有一串非核心副作用（统计投影、通知、
-- 风控、履约系统），它们不能和支付放在同一个数据库事务里，也不能"先提交
-- 订单再发消息"——进程在两次操作之间崩溃就会永久丢事件。Outbox 把"要发的
-- 事件"当成业务数据写进同一个事务，再由中继异步投递，语义是至少一次。

CREATE TABLE outbox_events (
    id              BIGSERIAL PRIMARY KEY,
    -- event_id 是消费侧幂等键：中继"投递成功但标记失败"时会重发同一条事件，
    -- 消费方靠它去重，而不是靠 broker 只投一次。
    event_id        UUID NOT NULL,
    event_type      TEXT NOT NULL
                    CONSTRAINT outbox_event_type_nonempty_check CHECK (btrim(event_type) <> ''),
    aggregate_type  TEXT NOT NULL
                    CONSTRAINT outbox_aggregate_type_nonempty_check CHECK (btrim(aggregate_type) <> ''),
    aggregate_id    BIGINT NOT NULL,
    -- partition_key 决定同一聚合的事件落到同一个分区/消费顺序里，
    -- 这里用作者 ID：作者日销投影必须按作者串行。
    partition_key   TEXT NOT NULL,
    payload         JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'PENDING'
                    CONSTRAINT outbox_status_check CHECK (status IN ('PENDING', 'PUBLISHED', 'DEAD')),
    attempts        INTEGER NOT NULL DEFAULT 0
                    CONSTRAINT outbox_attempts_nonnegative_check CHECK (attempts >= 0),
    -- available_at 兼作退避与租约到期时间：中继崩溃后租约自然过期，事件会被重投。
    available_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    leased_until    TIMESTAMPTZ,
    last_error      TEXT,
    published_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbox_event_id_key UNIQUE (event_id),
    CONSTRAINT outbox_published_at_check CHECK ((status = 'PUBLISHED') = (published_at IS NOT NULL))
);

-- 中继扫描：只取到期的 PENDING，配合 FOR UPDATE SKIP LOCKED 支持多实例并行。
CREATE INDEX idx_outbox_pending ON outbox_events (available_at, id) WHERE status = 'PENDING';
-- 运维排查：按聚合找事件；DLQ 列表。
CREATE INDEX idx_outbox_aggregate ON outbox_events (aggregate_type, aggregate_id, id DESC);
CREATE INDEX idx_outbox_dead ON outbox_events (id DESC) WHERE status = 'DEAD';

-- 消费回执：与业务投影在同一事务写入，这是"幂等消费"的实现方式——
-- 重复投递会撞主键，投影不会被应用第二次。
CREATE TABLE consumed_events (
    consumer      TEXT NOT NULL,
    event_id      UUID NOT NULL,
    event_type    TEXT NOT NULL,
    aggregate_id  BIGINT NOT NULL,
    consumed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE INDEX idx_consumed_events_aggregate ON consumed_events (aggregate_id, event_type);

-- 作者日销投影（事件驱动的读模型）。实时查询仍然是事实源，
-- 这张表是"最终一致"的物化视图，供对账任务比对。
CREATE TABLE author_sales_daily (
    author_id       BIGINT NOT NULL,
    stat_date       DATE NOT NULL,
    order_count     BIGINT NOT NULL DEFAULT 0
                    CONSTRAINT author_sales_order_count_nonnegative_check CHECK (order_count >= 0),
    units_sold      BIGINT NOT NULL DEFAULT 0
                    CONSTRAINT author_sales_units_nonnegative_check CHECK (units_sold >= 0),
    gmv             BIGINT NOT NULL DEFAULT 0
                    CONSTRAINT author_sales_gmv_nonnegative_check CHECK (gmv >= 0),
    refunded_orders BIGINT NOT NULL DEFAULT 0
                    CONSTRAINT author_sales_refunded_orders_nonnegative_check CHECK (refunded_orders >= 0),
    refunded_amount BIGINT NOT NULL DEFAULT 0
                    CONSTRAINT author_sales_refunded_amount_nonnegative_check CHECK (refunded_amount >= 0),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (author_id, stat_date)
);
