package mediaprobe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 这些用例调用**真实的 ffprobe/ffmpeg**（本机已安装），用 ffmpeg 现造测试素材。
// 媒体探测的全部价值在于"相信真实工具的输出"，用 fake 测等于没测。
// 没有装 ffmpeg 的环境会跳过，而不是失败。
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
}

// makeTestVideo 生成一段指定时长/分辨率的 mp4，并把 moov 放到尾部（默认行为），
// 用来验证"尾部 moov 会被识别并 remux"。
func makeTestVideo(t *testing.T, dir string, duration string, width, height int) string {
	t.Helper()
	path := filepath.Join(dir, "input.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration="+duration+":size="+itoa(width)+"x"+itoa(height)+":rate=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path)
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "generate fixture: %s", output)
	return path
}

func itoa(value int) string {
	return string(rune('0'+value/100)) + string(rune('0'+(value/10)%10)) + string(rune('0'+value%10))
}

func TestProbeRealFile(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := makeTestVideo(t, dir, "2", 320, 240)

	prober := NewProber()
	result, err := prober.Probe(context.Background(), path)
	require.NoError(t, err)
	require.InDelta(t, 2.0, result.DurationSeconds, 0.3)
	require.Equal(t, 320, result.Width)
	require.Equal(t, 240, result.Height)
	require.Equal(t, "h264", result.VideoCodec)
	require.False(t, result.HasAudio, "testsrc 没有音轨")
	require.Positive(t, result.SizeBytes)
}

// TestValidateEnforcesPolicy：策略校验是纯函数，逐条覆盖拒绝原因。
func TestValidateEnforcesPolicy(t *testing.T) {
	base := &Result{DurationSeconds: 10, Width: 1080, Height: 1920, VideoCodec: "h264", HasAudio: true}
	policy := DefaultPolicy()

	require.NoError(t, Validate(base, policy))

	cases := map[string]struct {
		mutate func(*Result)
		want   string
	}{
		"too long":  {func(r *Result) { r.DurationSeconds = 7200 }, "exceeds the maximum"},
		"too short": {func(r *Result) { r.DurationSeconds = 0.1 }, "shorter than the minimum"},
		"too wide":  {func(r *Result) { r.Width = 8192 }, "width"},
		"too tall":  {func(r *Result) { r.Height = 8192 }, "height"},
		"bad codec": {func(r *Result) { r.VideoCodec = "mpeg4" }, "not allowed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := *base
			tc.mutate(&result)
			err := Validate(&result, policy)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}

	// 可选音轨策略
	require.NoError(t, Validate(&Result{DurationSeconds: 5, VideoCodec: "h264"}, policy), "默认允许无音轨")
	require.Error(t, Validate(&Result{DurationSeconds: 5, VideoCodec: "h264"}, Policy{
		MaxDurationSeconds: 60, AllowedVideoCodecs: []string{"h264"}, RequireAudio: true,
	}), "要求音轨时必须拒绝纯视频")
}

// TestMoovDetectionAndFaststart 是这一项的核心价值：
// moov 在尾部的 mp4 必须被识别出来，remux 后 moov 必须到头部。
func TestMoovDetectionAndFaststart(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := makeTestVideo(t, dir, "2", 320, 240)

	prober := NewProber()
	before, err := prober.Probe(context.Background(), path)
	require.NoError(t, err)

	if before.MoovAtFront {
		// ffmpeg 的默认输出在不同版本上可能已经前置 moov；这种情况下
		// 用 -movflags +faststart 生成一个"确定在尾部"的样本再验证。
		t.Log("ffmpeg 默认已把 moov 前置，改用显式尾部布局验证 remux")
	}

	// 强制生成 moov 在尾部的文件（-movflags 不设置 faststart 且用 pipe 输出会前置，
	// 因此用 frag_keyframe 之外的默认布局：libx264 写 mp4 时 moov 在尾部）。
	tailPath := filepath.Join(dir, "tail.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "-faststart", tailPath)
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "generate tail-moov fixture: %s", output)

	tailResult, err := prober.Probe(context.Background(), tailPath)
	require.NoError(t, err)
	require.False(t, tailResult.MoovAtFront, "该样本的 moov 应在尾部")

	outPath := filepath.Join(dir, "faststart.mp4")
	require.NoError(t, prober.RemuxFaststart(context.Background(), tailPath, outPath))

	fixed, err := prober.Probe(context.Background(), outPath)
	require.NoError(t, err)
	require.True(t, fixed.MoovAtFront, "remux 后 moov 必须前置")
	// 内容不变：remux 是 -c copy，时长/分辨率/编码都不应改变。
	require.InDelta(t, tailResult.DurationSeconds, fixed.DurationSeconds, 0.2)
	require.Equal(t, tailResult.Width, fixed.Width)
	require.Equal(t, tailResult.VideoCodec, fixed.VideoCodec)
}

// TestProbeRejectsNonMedia：不是媒体的文件必须返回 ErrProbeFailed（可识别、可拒绝），
// 而不是让 worker 崩在解析里。
func TestProbeRejectsNonMedia(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-video.mp4")
	require.NoError(t, os.WriteFile(path, []byte("this is definitely not a video file"), 0o600))

	prober := NewProber()
	_, err := prober.Probe(context.Background(), path)
	require.ErrorIs(t, err, ErrProbeFailed)
}

// TestProbeMissingToolIsDistinguishable：没有装 ffprobe 时要能和"文件损坏"区分开——
// 前者应当降级为跳过校验并告警，后者应当拒绝视频。
func TestProbeMissingToolIsDistinguishable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "whatever.mp4")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	prober := &Prober{FFProbePath: "definitely-not-installed-ffprobe", Timeout: 2 * time.Second}
	_, err := prober.Probe(context.Background(), path)
	require.ErrorIs(t, err, ErrToolMissing)
}

// A killed command returns ExitError too; timeout must never mean invalid media.
func TestProbeTimeoutIsRetryable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ffprobe")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 5\n"), 0700))
	prober := &Prober{FFProbePath: path, Timeout: 20 * time.Millisecond}
	_, err := prober.Probe(context.Background(), "unused")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrProbeFailed)
}
