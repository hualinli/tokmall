-- 一个视频最多挂 5 个启用中的商品。应用层检查只能改善错误提示，不能
-- 防住并发请求或脚本直写，因此把最终不变量放在数据库触发器中。
-- advisory xact lock 以 video_id 为粒度串行化计数与写入，不锁整张表。
CREATE OR REPLACE FUNCTION enforce_video_product_active_limit()
RETURNS trigger AS $$
DECLARE
    active_count INTEGER;
BEGIN
    IF NEW.status <> 'ACTIVE' THEN
        RETURN NEW;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended('video-products:' || NEW.video_id::TEXT, 0));

    -- INSERT ... ON CONFLICT 的 INSERT trigger 先于冲突处理执行。视频已满时，
    -- 已 ACTIVE 的同一关系重放必须继续交给 UPSERT，而不是被误判成第 6 个。
    IF TG_OP = 'INSERT' AND EXISTS (
        SELECT 1 FROM video_products
         WHERE video_id = NEW.video_id
           AND product_id = NEW.product_id
           AND status = 'ACTIVE'
    ) THEN
        RETURN NEW;
    END IF;

    SELECT count(*)
      INTO active_count
      FROM video_products
     WHERE video_id = NEW.video_id
       AND status = 'ACTIVE'
       AND id <> COALESCE(NEW.id, 0);

    IF active_count >= 5 THEN
        RAISE EXCEPTION 'video % already has 5 active products', NEW.video_id
            USING ERRCODE = '23514',
                  CONSTRAINT = 'video_products_active_limit_check';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_video_products_active_limit
BEFORE INSERT OR UPDATE OF video_id, status ON video_products
FOR EACH ROW EXECUTE FUNCTION enforce_video_product_active_limit();
