package storage

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/config"
)

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMinIOUploadSnapshotAndSizeBound(t *testing.T) {
	if os.Getenv("TOKMALL_RUN_MINIO_INTEGRATION") != "1" {
		t.Skip("set TOKMALL_RUN_MINIO_INTEGRATION=1 to run against local MinIO")
	}
	cfg := config.MinIOConfig{Endpoint: "localhost:9000", AccessKey: "tokmall", SecretKey: "tokmall123", Bucket: "review-" + uuid.NewString(), UploadURLExpiry: time.Minute}
	c, err := New(cfg)
	require.NoError(t, err)
	ctx := context.Background()
	source, dest := "staging/video.mp4", "videos/video.mp4"
	t.Cleanup(func() {
		_ = c.mc.RemoveObject(ctx, c.bucket, source, minio.RemoveObjectOptions{})
		_ = c.mc.RemoveObject(ctx, c.bucket, dest, minio.RemoveObjectOptions{})
		_ = c.mc.RemoveBucket(ctx, c.bucket)
	})
	url, err := c.PresignPutURL(ctx, source, 8, "video/mp4")
	require.NoError(t, err)
	put := func(body string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "video/mp4")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	read := func() string {
		obj, err := c.mc.GetObject(ctx, c.bucket, dest, minio.GetObjectOptions{})
		require.NoError(t, err)
		defer func() { _ = obj.Close() }()
		body, err := io.ReadAll(obj)
		require.NoError(t, err)
		return string(body)
	}
	put("original")
	require.NoError(t, c.FinalizeUpload(ctx, source, dest, 64))
	put("replaced")
	require.Equal(t, "original", read(), "replaying the original PUT changes only staging")
	oversized, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(strings.Repeat("x", 65)))
	require.NoError(t, err)
	oversized.Header.Set("Content-Type", "video/mp4")
	resp, err := http.DefaultClient.Do(oversized)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "PUT must bind the declared length")
	_ = resp.Body.Close()
	_, err = c.mc.PutObject(ctx, c.bucket, source, strings.NewReader(strings.Repeat("x", 65)), 65, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.ErrorIs(t, c.FinalizeUpload(ctx, source, dest, 64), ErrInvalidObjectSize)
	require.Equal(t, "original", read(), "oversized objects cannot replace final content")

	// Rewrite after StatObject but before CopyObject: copy must reject the new ETag.
	put("snapshot")
	raw := c.mc
	hooked, err := minio.New(cfg.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-Amz-Copy-Source") != "" {
			_, err := raw.PutObject(ctx, c.bucket, source, strings.NewReader(strings.Repeat("z", 65)), 65, minio.PutObjectOptions{})
			if err != nil {
				return nil, err
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})})
	require.NoError(t, err)
	c.mc = hooked
	require.Error(t, c.FinalizeUpload(ctx, source, dest, 64), "concurrent source replacement must fail the ETag precondition")
	c.mc = raw
	require.Equal(t, "original", read())
}
