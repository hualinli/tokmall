-- 播放事件表：V1 同步写入（无 Kafka），M5 异步化后由 worker 落库。
-- event_id 客户端生成（UUID v7），唯一约束提供幂等去重（网络重试不重复计数）。
-- FK 评估（ADR-013）：保留。播放写入频率高于 likes，但当前量级每秒个位数，
-- 若压测出现锁等待/死锁再删 FK + 对账任务。
CREATE TABLE play_events (
  event_id UUID PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id),
  video_id BIGINT NOT NULL REFERENCES videos(id),
  session_id VARCHAR(128),
  played_at TIMESTAMPTZ NOT NULL,
  trace_id VARCHAR(64),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 播放计数聚合（M8 排序的分数来源）：按 video 统计播放量。
CREATE INDEX idx_play_events_video_time
  ON play_events (video_id, played_at);
