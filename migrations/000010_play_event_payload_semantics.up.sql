-- 区分客户端显式提供 played_at 与服务端补齐的接收时间。
-- 仅比较显式客户端时间，才能让省略 played_at 的网络重试保持幂等，
-- 同时避免把“显式时间”和“省略时间”两种不同业务载荷错误合并。
ALTER TABLE play_events
  ADD COLUMN played_at_provided BOOLEAN NOT NULL DEFAULT false;
