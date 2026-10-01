-- 视频媒体校验（ffprobe）结果。
--
-- 背景：上传走预签名 PUT 直传，服务端只做 StatObject（存在且非空），
-- 客户端声明的时长/分辨率完全不可信，非视频文件也能进播放链路。
-- 这里记录**服务端实测**的结果，并把不合格的视频挡在发布之前。
--
-- 为什么用状态列而不是布尔：需要区分"还没校验"与"校验通过/拒绝"，
-- 否则 worker 无法知道该处理哪些行（PENDING 就是工作队列）。
ALTER TABLE videos
    ADD COLUMN media_probe_status TEXT NOT NULL DEFAULT 'PENDING',
    ADD COLUMN media_probe_error TEXT,
    -- 服务端实测值（与客户端声明的 duration_seconds 分开存：
    -- 出问题时能对比"用户说多长"与"实际多长"）。
    ADD COLUMN probed_duration_seconds NUMERIC(10, 3),
    ADD COLUMN probed_width INTEGER,
    ADD COLUMN probed_height INTEGER,
    ADD COLUMN probed_video_codec TEXT,
    ADD COLUMN probed_at TIMESTAMPTZ,
    -- faststart 是否已处理（moov 前置）。false 表示首帧需要拉完整个文件。
    ADD COLUMN faststart_applied BOOLEAN NOT NULL DEFAULT false;

-- 已有视频跳过校验：它们是这个能力上线前上传的，不应该让 worker
-- 突然去处理整个历史存量（既没必要，也会打满对象存储带宽）。
UPDATE videos SET media_probe_status = 'SKIPPED';

ALTER TABLE videos
    ADD CONSTRAINT videos_media_probe_status_check
        CHECK (media_probe_status IN ('PENDING', 'PASSED', 'REJECTED', 'SKIPPED')),
    ADD CONSTRAINT videos_probed_duration_check CHECK (
        probed_duration_seconds IS NULL
        OR (probed_duration_seconds > 0 AND probed_duration_seconds <= 86400)
    ),
    -- 状态与时间戳必须同真同假：PASSED/REJECTED 必须有 probed_at，
    -- PENDING/SKIPPED 必须没有——避免出现"已校验但没有结果"的无法解释状态。
    ADD CONSTRAINT videos_probe_state_check CHECK (
        (media_probe_status IN ('PENDING', 'SKIPPED') AND probed_at IS NULL)
        OR (media_probe_status IN ('PASSED', 'REJECTED') AND probed_at IS NOT NULL)
    ),
    -- 被拒绝的视频不允许处于已发布状态（否则会继续出现在 Feed 里）。
    ADD CONSTRAINT videos_rejected_not_published_check
        CHECK (NOT (media_probe_status = 'REJECTED' AND status = 'PUBLISHED'));

-- worker 的取件索引：只扫 PENDING，按 id 升序（先进先出）。
-- 部分索引让"已处理完的历史视频"完全不参与索引扫描。
CREATE INDEX idx_videos_media_probe_pending
    ON videos (id) WHERE media_probe_status = 'PENDING';
