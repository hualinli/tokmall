// Package mediaprobe 用 ffprobe/ffmpeg 校验上传的视频，并把 moov 前置（faststart）。
//
// 为什么需要它：TokMall 的上传是**预签名 PUT 直传**——客户端拿到 URL 后直接写对象存储，
// 服务端只做 StatObject（文件存在且非空）。这意味着：
//
//   - 客户端声明的时长/分辨率完全不可信（Create 接口只校验 1..3600 的入参范围）；
//   - 上传一个不是视频的文件、或一个 10 小时的文件，都能进播放链路；
//   - 很多编码器把 moov atom 写在文件尾部，播放器必须先拉完整个文件才能出首帧——
//     预签名 GET 直连对象存储时这个代价直接落在用户身上。
//
// 同类项目（GCFeed）在**上传请求里同步**跑 ffprobe(30s 超时) + ffmpeg(2min 超时)，
// 阻塞 HTTP 线程且依赖宿主机装了 ffmpeg。本包只提供**纯函数式的能力**
// （探测 / 校验 / remux），由后台 worker 异步调用，不阻塞用户请求。
package mediaprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrProbeFailed 表示 ffprobe 无法解析该文件（不是视频 / 文件损坏）。
	ErrProbeFailed = errors.New("media probe failed")
	// ErrToolMissing 表示宿主机没有安装 ffprobe/ffmpeg。
	// 调用方应当据此降级为"跳过校验"并告警，而不是把视频标成损坏。
	ErrToolMissing = errors.New("ffprobe/ffmpeg not installed")
)

// Result 是探测结果（只保留我们真正会用到的字段）。
type Result struct {
	DurationSeconds float64
	Width           int
	Height          int
	VideoCodec      string
	AudioCodec      string
	HasAudio        bool
	SizeBytes       int64
	// MoovAtFront 表示 moov atom 已经在文件头部（可以直接流式播放）。
	// 判断方式见 moovAtFront：读文件头部的 box 序列。
	MoovAtFront bool
}

// Policy 是媒体准入策略。所有阈值都显式配置，不写死在代码里——
// "多长算太长""哪些编码器可用"是产品决策，不是实现细节。
type Policy struct {
	MaxDurationSeconds float64
	MinDurationSeconds float64
	MaxWidth           int
	MaxHeight          int
	AllowedVideoCodecs []string
	// RequireAudio=false 表示允许纯视频（短视频常见）。
	RequireAudio bool
}

// DefaultPolicy 是默认准入策略：最长 1 小时、最大 4096×4096、只允许浏览器普遍支持的编码。
func DefaultPolicy() Policy {
	return Policy{
		MaxDurationSeconds: 3600,
		MinDurationSeconds: 0.5,
		MaxWidth:           4096,
		MaxHeight:          4096,
		AllowedVideoCodecs: []string{"h264", "hevc", "av1", "vp8", "vp9"},
	}
}

// Prober 封装外部命令调用，便于测试注入假的实现。
type Prober struct {
	FFProbePath string
	FFmpegPath  string
	Timeout     time.Duration
}

func NewProber() *Prober {
	return &Prober{FFProbePath: "ffprobe", FFmpegPath: "ffmpeg", Timeout: 30 * time.Second}
}

// ffprobeOutput 是 `ffprobe -print_format json -show_format -show_streams` 的输出子集。
type ffprobeOutput struct {
	Format struct {
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
}

// Probe 读取媒体元数据。path 必须是本地文件路径（worker 负责从对象存储下载）。
func (p *Prober) Probe(ctx context.Context, path string) (*Result, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	//nolint:gosec // 命令名与参数都由本包构造，path 是 worker 下载到受控临时目录的路径
	cmd := exec.CommandContext(probeCtx, p.binary(p.FFProbePath),
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	output, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrToolMissing
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// ffprobe 非 0 退出：文件不是可识别的媒体。
			return nil, fmt.Errorf("%w: %s", ErrProbeFailed, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("run ffprobe: %w", err)
	}

	var parsed ffprobeOutput
	if err := json.Unmarshal(output, &parsed); err != nil {
		return nil, fmt.Errorf("%w: parse ffprobe output: %w", ErrProbeFailed, err)
	}

	result := &Result{}
	for _, stream := range parsed.Streams {
		switch stream.CodecType {
		case "video":
			if result.VideoCodec == "" { // 只取第一条视频流
				result.VideoCodec = stream.CodecName
				result.Width = stream.Width
				result.Height = stream.Height
			}
		case "audio":
			if !result.HasAudio {
				result.HasAudio = true
				result.AudioCodec = stream.CodecName
			}
		}
	}
	if result.VideoCodec == "" {
		return nil, fmt.Errorf("%w: no video stream", ErrProbeFailed)
	}
	// duration 优先取 format（容器级），回退到流级；解析失败按 0 处理，由策略拒绝。
	if duration, err := strconv.ParseFloat(strings.TrimSpace(parsed.Format.Duration), 64); err == nil {
		result.DurationSeconds = duration
	}
	if size, err := strconv.ParseInt(strings.TrimSpace(parsed.Format.Size), 10, 64); err == nil {
		result.SizeBytes = size
	}
	result.MoovAtFront = moovAtFront(path)
	return result, nil
}

// Validate 用策略校验探测结果，返回人类可读的拒绝原因。
func Validate(result *Result, policy Policy) error {
	if result == nil {
		return errors.New("no probe result")
	}
	if policy.MinDurationSeconds > 0 && result.DurationSeconds < policy.MinDurationSeconds {
		return fmt.Errorf("duration %.2fs is shorter than the minimum %.2fs",
			result.DurationSeconds, policy.MinDurationSeconds)
	}
	if policy.MaxDurationSeconds > 0 && result.DurationSeconds > policy.MaxDurationSeconds {
		return fmt.Errorf("duration %.2fs exceeds the maximum %.0fs",
			result.DurationSeconds, policy.MaxDurationSeconds)
	}
	if policy.MaxWidth > 0 && result.Width > policy.MaxWidth {
		return fmt.Errorf("width %d exceeds the maximum %d", result.Width, policy.MaxWidth)
	}
	if policy.MaxHeight > 0 && result.Height > policy.MaxHeight {
		return fmt.Errorf("height %d exceeds the maximum %d", result.Height, policy.MaxHeight)
	}
	if len(policy.AllowedVideoCodecs) > 0 && !contains(policy.AllowedVideoCodecs, result.VideoCodec) {
		return fmt.Errorf("video codec %q is not allowed (allowed: %s)",
			result.VideoCodec, strings.Join(policy.AllowedVideoCodecs, ", "))
	}
	if policy.RequireAudio && !result.HasAudio {
		return errors.New("audio stream is required")
	}
	return nil
}

// RemuxFaststart 把 moov atom 移到文件头部（`-c copy` 不重新编码，因此很快）。
//
// 为什么值得做：moov 在尾部的 mp4 必须被完整下载才能播放首帧。短视频场景下
// 用户对首帧延迟最敏感，而这一步只是重写容器、不动编码数据。
func (p *Prober) RemuxFaststart(ctx context.Context, inputPath, outputPath string) error {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// ffmpeg 的 remux 比 ffprobe 慢，给更宽松的超时（同类项目给 2 分钟）。
	remuxCtx, cancel := context.WithTimeout(ctx, timeout*4)
	defer cancel()

	//nolint:gosec // 同上：参数由本包构造
	cmd := exec.CommandContext(remuxCtx, p.binary(p.FFmpegPath),
		"-y", "-v", "error", "-i", inputPath,
		"-map", "0", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", outputPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return ErrToolMissing
		}
		return fmt.Errorf("remux faststart: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p *Prober) binary(name string) string {
	if name == "" {
		return "ffprobe"
	}
	return name
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
