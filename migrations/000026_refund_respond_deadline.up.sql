-- 退款处理时限：respond_by 是申请那一刻就确定的商家处理截止时间。
-- 之后改配置不能改已经写下来的期限，所以不在查询里用 requested_at + 窗口现算。
-- decision_source 区分商家裁决和超时自动同意。超时同意没有裁决人，decided_by 为空。
--
-- 回填的 2 天与 order.DefaultRefundRespondWindow（48h）是同一个数。
-- 已裁决的行只是历史，按申请时间回填。
-- 仍在 REQUESTED 的行从上线时刻起再给 2 天：商家当时不知道有这个时限，
-- 不能把两天前的申请在 worker 第一次扫描时直接同意并退钱。

ALTER TABLE refunds
    ADD COLUMN respond_by TIMESTAMPTZ,
    ADD COLUMN decision_source TEXT;

UPDATE refunds SET respond_by = CASE
    WHEN status = 'REQUESTED' THEN GREATEST(requested_at, now()) + INTERVAL '2 days'
    ELSE requested_at + INTERVAL '2 days'
END;
UPDATE refunds SET decision_source = 'MERCHANT' WHERE status <> 'REQUESTED';

ALTER TABLE refunds ALTER COLUMN respond_by SET NOT NULL;

ALTER TABLE refunds DROP CONSTRAINT refunds_decision_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_decision_check CHECK (
    (status = 'REQUESTED') = (decided_at IS NULL)
    AND (status = 'REQUESTED') = (decision_source IS NULL)
    AND (status = 'SUCCEEDED') = (refunded_at IS NOT NULL)
    AND (decision_remark IS NULL OR btrim(decision_remark) <> '')
    -- CASE 不用 OR：decision_source 为 NULL 时，= 'MERCHANT' 的结果是 NULL，
    -- OR 起来仍可能是 NULL，而 CHECK 只拒绝 FALSE、会放行 NULL。
    -- NULL 一定进 ELSE，得到确定的 TRUE 或 FALSE。
    AND CASE decision_source
        WHEN 'MERCHANT' THEN decided_by IS NOT NULL
        WHEN 'TIMEOUT' THEN decided_by IS NULL AND status = 'SUCCEEDED'
        ELSE decision_source IS NULL AND decided_by IS NULL
    END
);

ALTER TABLE refunds ADD CONSTRAINT refunds_respond_by_check CHECK (
    respond_by > requested_at
);

CREATE INDEX idx_refunds_respond_due
    ON refunds (respond_by, id)
    WHERE status = 'REQUESTED';
