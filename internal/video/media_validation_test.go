package video

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaValidationVisibilityAndPublishPermission(t *testing.T) {
	for _, status := range []MediaProbeStatus{ProbePending, ProbePassed, ProbeRejected, ProbeSkipped} {
		t.Run(string(status), func(t *testing.T) {
			repo := newFakeRepo()
			raw := "not a playable video: /private/media/token-secret?signature=secret"
			repo.videos[1] = &Video{ID: 1, AuthorID: 42, Status: StatusPendingReview, MediaProbeStatus: status, MediaProbeError: &raw, ObjectKey: "videos/42/test.mp4"}
			svc := newTestService(repo, newFakeStorage())
			detail, err := svc.Get(context.Background(), 42, 1)
			require.NoError(t, err)
			require.NotNil(t, detail.MediaValidation)
			require.Equal(t, status, detail.MediaValidation.Status)
			require.Equal(t, status == ProbePassed || status == ProbeSkipped, detail.MediaValidation.CanPublish)
			list, err := svc.ListMine(context.Background(), 42, 20, "")
			require.NoError(t, err)
			require.Len(t, list.Items, 1)
			require.Equal(t, detail.MediaValidation, list.Items[0].MediaValidation)
			body, err := json.Marshal(detail)
			require.NoError(t, err)
			require.NotContains(t, string(body), raw)
			if status == ProbeRejected {
				require.Equal(t, "无法读取视频，请检查文件是否损坏并重新导出。", *detail.MediaValidation.Reason)
			} else {
				require.Nil(t, detail.MediaValidation.Reason)
			}
			_, err = svc.Get(context.Background(), 7, 1)
			require.ErrorIs(t, err, ErrNotFound)
			repo.videos[1].Status = StatusPublished
			// 已发布视频不应再次提供发布操作；非作者连校验字段都不可见。
			own, err := svc.Get(context.Background(), 42, 1)
			require.NoError(t, err)
			require.False(t, own.MediaValidation.CanPublish)
			for _, viewer := range []int64{0, 7} {
				public, err := svc.Get(context.Background(), viewer, 1)
				require.NoError(t, err)
				require.Nil(t, public.MediaValidation)
				body, err := json.Marshal(public)
				require.NoError(t, err)
				require.NotContains(t, string(body), "media_validation")
			}
		})
	}
}

func TestMediaValidationSafeReasons(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"video exceeds the object size limit", "视频文件过大"},
		{"duration 0.2s is shorter than the minimum 1s", "时长过短"},
		{"duration 4000s exceeds the maximum 3600s", "时长超过上限"},
		{"width 9999 exceeds the maximum 1920", "分辨率超过上限"},
		{"height 9999 exceeds the maximum 1080", "分辨率超过上限"},
		{"video codec \"unsafe-path\" is not allowed", "编码不受支持"},
		{"audio stream is required", "缺少音轨"},
		{"unknown error /private/secret", "视频未通过校验"},
	}
	for _, tc := range cases {
		v := &Video{Status: StatusPendingReview, MediaProbeStatus: ProbeRejected, MediaProbeError: &tc.raw}
		view := mediaValidationView(v)
		require.False(t, view.CanPublish)
		require.NotNil(t, view.Reason)
		require.Contains(t, *view.Reason, tc.want)
		require.NotContains(t, *view.Reason, tc.raw)
	}
}
