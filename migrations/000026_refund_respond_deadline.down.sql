-- 超时同意的 decided_by 为空。旧约束要求已裁决行必须有裁决人，
-- 直接恢复会失败，而且删掉 decision_source 后无法分辨这是系统同意还是脏数据。
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM refunds WHERE decision_source = 'TIMEOUT') THEN
        RAISE EXCEPTION 'cannot roll back refund response deadline while TIMEOUT decisions exist';
    END IF;
END $$;

DROP INDEX IF EXISTS idx_refunds_respond_due;
ALTER TABLE refunds DROP CONSTRAINT IF EXISTS refunds_respond_by_check;
ALTER TABLE refunds DROP CONSTRAINT refunds_decision_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_decision_check CHECK (
    (status = 'REQUESTED') = (decided_at IS NULL)
    AND (status = 'REQUESTED') = (decided_by IS NULL)
    AND (status = 'SUCCEEDED') = (refunded_at IS NOT NULL)
    AND (decision_remark IS NULL OR btrim(decision_remark) <> '')
);

ALTER TABLE refunds DROP COLUMN decision_source;
ALTER TABLE refunds DROP COLUMN respond_by;
