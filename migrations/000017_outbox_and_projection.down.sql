-- 回滚 Outbox 与投影。存在未投递事件或消费回执时拒绝回滚：
-- 这些记录代表"已经发生但尚未被下游看到"的事实，静默删除等于丢事件。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM outbox_events WHERE status <> 'PUBLISHED') THEN
        RAISE EXCEPTION 'outbox has undelivered events; drain or requeue them before 000017 rollback';
    END IF;
END $$;

DROP TABLE author_sales_daily;
DROP TABLE consumed_events;
DROP TABLE outbox_events;
