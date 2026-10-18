-- 发货后自动确认收货：auto_complete_at 是"系统替买家确认"的截止时间。
--
-- 发货时与 fulfilling_at 一起写入，之后只会被"拒绝退款并退回 FULFILLING"顺延，
-- 不会被清空：它和 fulfilling_at 一样是履约阶段的历史事实。
-- 截止时间可变，所以不用延迟事件（发出去收不回），由轮询 worker 按索引扫描到期订单。
ALTER TABLE orders ADD COLUMN auto_complete_at TIMESTAMPTZ;

-- 存量已发货订单按默认 7 天回填；进程配置的窗口只影响之后发货的订单。
UPDATE orders SET auto_complete_at = fulfilling_at + INTERVAL '7 days'
WHERE fulfilling_at IS NOT NULL;

ALTER TABLE orders ADD CONSTRAINT orders_auto_complete_check CHECK (
    (fulfilling_at IS NULL) = (auto_complete_at IS NULL)
    AND (auto_complete_at IS NULL OR auto_complete_at > fulfilling_at)
);

CREATE INDEX idx_orders_auto_complete_due
    ON orders (auto_complete_at, id)
    WHERE status = 'FULFILLING';
