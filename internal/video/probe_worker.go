package video

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/mediaprobe"
)

// 媒体校验 worker
//
// 上传是预签名 PUT 直传：文件从客户端直接进对象存储，服务端在 Create 时
// 只做 StatObject。因此**客户端声明的时长/分辨率不可信**，非视频文件也能落库。
//
// 这个 worker 把校验放到**异步**侧（同类项目在上传请求里同步跑 ffprobe+ffmpeg，
// 阻塞 HTTP 线程 30s~2min，且失败直接 500）：
//
//	取 PENDING 视频（SKIP LOCKED）→ 下载到临时目录 → ffprobe 实测
//	→ 策略校验（时长/分辨率/编码）→ 需要时 remux 加 faststart 回写
//	→ 写回实测结果与状态（PASSED / REJECTED / SKIPPED）
//
// 三条设计原则：
//  1. **不阻塞用户请求**：校验慢只影响"视频什么时候可发布"，不影响上传体验；
//  2. **工具缺失 ≠ 文件损坏**：没装 ffprobe 时标记 SKIPPED 并告警（降级），
//     绝不把正常视频误判为坏文件；
//  3. **校验结果落库**：实测时长与客户端声明分开存，便于事后对账与排查
//     （"用户说 30 秒、实际 3 小时"这类问题必须能查到）。
type ProbeWorker struct {
	repo     ProbeRepository
	storage  ProbeStorage
	prober   *mediaprobe.Prober
	policy   mediaprobe.Policy
	interval time.Duration
	batch    int
	stats    ProbeStats
	// workDir 是临时目录根：每个视频一个子目录，处理完删除。
	workDir string
}

// MediaProbePolicy 把配置翻译成媒体准入策略。
// 放在这里而不是 config 包：config 只描述"环境变量长什么样"，
// 策略的领域含义属于 video 模块。
func MediaProbePolicy(cfg *config.Config) mediaprobe.Policy {
	policy := mediaprobe.DefaultPolicy()
	if cfg == nil {
		return policy
	}
	if cfg.MediaMaxDuration > 0 {
		policy.MaxDurationSeconds = cfg.MediaMaxDuration
	}
	if cfg.MediaMaxDimension > 0 {
		policy.MaxWidth = cfg.MediaMaxDimension
		policy.MaxHeight = cfg.MediaMaxDimension
	}
	if len(cfg.MediaAllowedCodecs) > 0 {
		policy.AllowedVideoCodecs = cfg.MediaAllowedCodecs
	}
	return policy
}

// ProbeRepository 是 worker 需要的存储能力（消费侧定义）。
type ProbeRepository interface {
	// ListPendingProbe 取一批待校验视频（PENDING），按 id 升序。
	ListPendingProbe(ctx context.Context, limit int) ([]ProbeCandidate, error)
	// SaveProbeResult 写回校验结果。返回 false 表示该行已被别的 worker 处理。
	SaveProbeResult(ctx context.Context, videoID int64, result ProbeOutcome) (bool, error)
}

// ProbeCandidate 是一条待校验视频的关键信息。
type ProbeCandidate struct {
	VideoID   int64
	ObjectKey string
	// DeclaredDurationSeconds 是客户端声明的时长，用于和实测值比对。
	DeclaredDurationSeconds int
}

// ProbeOutcome 是校验结果。
type ProbeOutcome struct {
	Status           string // PASSED / REJECTED / SKIPPED
	Error            string
	DurationSeconds  float64
	Width            int
	Height           int
	VideoCodec       string
	FaststartApplied bool
}

// ProbeStorage 是对象存储的最小能力（下载 / 回写 / 删除）。
type ProbeStorage interface {
	DownloadToFile(ctx context.Context, objectKey, path string) error
	UploadFromFile(ctx context.Context, objectKey, path string, size int64) error
}

// ProbeStats 是指标接口（observability 实现）。
type ProbeStats interface {
	ProbeResult(status string)
	ProbeDuration(seconds float64)
	FaststartApplied()
	ProbeSkipped(reason string)
}

type nopProbeStats struct{}

func (nopProbeStats) ProbeResult(string)    {}
func (nopProbeStats) ProbeDuration(float64) {}
func (nopProbeStats) FaststartApplied()     {}
func (nopProbeStats) ProbeSkipped(string)   {}

type ProbeOption func(*ProbeWorker)

func WithProbePolicy(policy mediaprobe.Policy) ProbeOption {
	return func(w *ProbeWorker) { w.policy = policy }
}

func WithProbeStats(stats ProbeStats) ProbeOption {
	return func(w *ProbeWorker) { w.stats = stats }
}

func WithProbeInterval(interval time.Duration) ProbeOption {
	return func(w *ProbeWorker) {
		if interval > 0 {
			w.interval = interval
		}
	}
}

func WithProbeBatch(batch int) ProbeOption {
	return func(w *ProbeWorker) {
		if batch > 0 {
			w.batch = batch
		}
	}
}

func NewProbeWorker(repo ProbeRepository, storage ProbeStorage, opts ...ProbeOption) *ProbeWorker {
	worker := &ProbeWorker{
		repo:     repo,
		storage:  storage,
		prober:   mediaprobe.NewProber(),
		policy:   mediaprobe.DefaultPolicy(),
		interval: 15 * time.Second,
		batch:    4,
		stats:    nopProbeStats{},
		workDir:  os.TempDir(),
	}
	for _, opt := range opts {
		opt(worker)
	}
	return worker
}

// Run 阻塞运行直到 ctx 取消。
func (w *ProbeWorker) Run(ctx context.Context) {
	if w == nil || w.repo == nil || w.storage == nil {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

// RunOnce 处理一批待校验视频。返回处理条数（测试与排障用）。
func (w *ProbeWorker) RunOnce(ctx context.Context) int {
	candidates, err := w.repo.ListPendingProbe(ctx, w.batch)
	if err != nil {
		slog.Warn("list pending media probes failed", "err", err)
		return 0
	}
	processed := 0
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return processed
		}
		if err := w.probeOne(ctx, candidate); err != nil {
			slog.Warn("media probe failed", "video_id", candidate.VideoID, "err", err)
		}
		processed++
	}
	return processed
}

func (w *ProbeWorker) probeOne(ctx context.Context, candidate ProbeCandidate) error {
	dir, err := os.MkdirTemp(w.workDir, fmt.Sprintf("tokmall-probe-%d-", candidate.VideoID))
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	// 临时目录必须清理：媒体文件动辄几十上百 MB，泄漏会打满磁盘。
	defer func() { _ = os.RemoveAll(dir) }()

	inputPath := filepath.Join(dir, "input.mp4")
	if err := w.storage.DownloadToFile(ctx, candidate.ObjectKey, inputPath); err != nil {
		// 下载失败通常是对象存储暂时不可用：**不写结果**，留给下一轮重试。
		return fmt.Errorf("download object: %w", err)
	}

	result, err := w.prober.Probe(ctx, inputPath)
	if err != nil {
		switch {
		case errors.Is(err, mediaprobe.ErrToolMissing):
			// 工具缺失是环境问题，不是文件问题：标记 SKIPPED（降级放行）并告警，
			// 绝不能把正常视频误判成坏文件。
			slog.Error("ffprobe not available, skipping media validation", "video_id", candidate.VideoID)
			w.stats.ProbeSkipped("tool_missing")
			return w.save(ctx, candidate.VideoID, ProbeOutcome{Status: "SKIPPED", Error: "ffprobe not installed"})
		case errors.Is(err, mediaprobe.ErrProbeFailed):
			w.stats.ProbeResult("rejected")
			return w.save(ctx, candidate.VideoID, ProbeOutcome{
				Status: "REJECTED",
				Error:  fmt.Sprintf("not a playable video: %v", err),
			})
		default:
			return err // 超时/取消等：不写结果，下轮重试
		}
	}

	w.stats.ProbeDuration(result.DurationSeconds)

	if policyErr := mediaprobe.Validate(result, w.policy); policyErr != nil {
		w.stats.ProbeResult("rejected")
		return w.save(ctx, candidate.VideoID, ProbeOutcome{
			Status:          "REJECTED",
			Error:           policyErr.Error(),
			DurationSeconds: result.DurationSeconds,
			Width:           result.Width,
			Height:          result.Height,
			VideoCodec:      result.VideoCodec,
		})
	}

	outcome := ProbeOutcome{
		Status:          "PASSED",
		DurationSeconds: result.DurationSeconds,
		Width:           result.Width,
		Height:          result.Height,
		VideoCodec:      result.VideoCodec,
	}

	// faststart：moov 在尾部时重写容器（-c copy，不重新编码）。
	// 失败不影响校验结论：只是首帧会慢一些，属于"降级但仍可播"。
	if !result.MoovAtFront {
		faststartPath := filepath.Join(dir, "faststart.mp4")
		if err := w.prober.RemuxFaststart(ctx, inputPath, faststartPath); err != nil {
			slog.Warn("faststart remux failed, keeping original object",
				"video_id", candidate.VideoID, "err", err)
		} else if info, statErr := os.Stat(faststartPath); statErr == nil {
			if uploadErr := w.storage.UploadFromFile(ctx, candidate.ObjectKey, faststartPath, info.Size()); uploadErr != nil {
				slog.Warn("upload faststart object failed, keeping original",
					"video_id", candidate.VideoID, "err", uploadErr)
			} else {
				outcome.FaststartApplied = true
				w.stats.FaststartApplied()
			}
		}
	}

	w.stats.ProbeResult("passed")
	return w.save(ctx, candidate.VideoID, outcome)
}

func (w *ProbeWorker) save(ctx context.Context, videoID int64, outcome ProbeOutcome) error {
	saved, err := w.repo.SaveProbeResult(ctx, videoID, outcome)
	if err != nil {
		return fmt.Errorf("save probe result: %w", err)
	}
	if !saved {
		// 行已被别的 worker 处理（多实例部署）：不是错误。
		slog.Debug("media probe result already saved by another worker", "video_id", videoID)
	}
	return nil
}
