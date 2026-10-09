package storage

import (
	"context"
	"errors"
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

func s3IntegrationClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("TOKMALL_RUN_S3_INTEGRATION") != "1" {
		t.Skip("set TOKMALL_RUN_S3_INTEGRATION=1 to run against local S3-compatible storage")
	}
	cfg := config.S3Config{Endpoint: "localhost:9000", AccessKey: "tokmall", SecretKey: "tokmall123", Bucket: "review-" + uuid.NewString(), UploadURLExpiry: time.Minute}
	for _, setting := range []struct {
		key    string
		target *string
	}{{"S3_ENDPOINT", &cfg.Endpoint}, {"S3_ACCESS_KEY", &cfg.AccessKey}, {"S3_SECRET_KEY", &cfg.SecretKey}} {
		if value := os.Getenv(setting.key); value != "" {
			*setting.target = value
		}
	}
	c, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.Background()
		for obj := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = c.mc.RemoveObject(ctx, c.bucket, obj.Key, minio.RemoveObjectOptions{})
		}
		_ = c.mc.RemoveBucket(ctx, c.bucket)
	})
	return c
}

func TestS3UploadSnapshotAndSizeBound(t *testing.T) {
	c := s3IntegrationClient(t)
	ctx := context.Background()
	source, dest := "staging/video.mp4", "videos/video.mp4"
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
	wrongType, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader("original"))
	require.NoError(t, err)
	wrongType.Header.Set("Content-Type", "image/jpeg")
	wrongResp, err := http.DefaultClient.Do(wrongType)
	require.NoError(t, err)
	_ = wrongResp.Body.Close()
	require.Equal(t, http.StatusForbidden, wrongResp.StatusCode, "PUT must bind the declared content type")
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
	rawCreds, err := raw.GetCreds()
	require.NoError(t, err)
	hooked, err := minio.New(raw.EndpointURL().Host, &minio.Options{Creds: credentials.NewStaticV4(rawCreds.AccessKeyID, rawCreds.SecretAccessKey, rawCreds.SessionToken), Secure: raw.EndpointURL().Scheme == "https", Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
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
	err = c.FinalizeUpload(ctx, source, dest, 64)
	require.Error(t, err, "concurrent source replacement must fail the ETag precondition")
	require.Equal(t, "PreconditionFailed", minio.ToErrorResponse(errors.Unwrap(err)).Code)
	c.mc = raw
	require.Equal(t, "original", read())
}

func TestS3BrowserAndWorkerContract(t *testing.T) {
	c := s3IntegrationClient(t)
	ctx := context.Background()
	key, body := "videos/worker.mp4", "worker-media-content"
	file := t.TempDir() + "/source.mp4"
	require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	require.NoError(t, c.UploadFromFile(ctx, key, file, int64(len(body))))
	download := t.TempDir() + "/download.mp4"
	require.NoError(t, c.DownloadToFile(ctx, key, download))
	bytes, err := os.ReadFile(download)
	require.NoError(t, err)
	require.Equal(t, body, string(bytes))
	url, err := c.PresignGetURLWithExpiry(ctx, key, time.Minute)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=0-5")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	bytes, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, "worker", string(bytes))
	req, err = http.NewRequestWithContext(ctx, http.MethodOptions, url, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "range")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Less(t, resp.StatusCode, 300)
	require.Equal(t, "http://localhost:5173", resp.Header.Get("Access-Control-Allow-Origin"))
	expiringURL, err := c.PresignGetURLWithExpiry(ctx, key, time.Second)
	require.NoError(t, err)
	time.Sleep(2 * time.Second)
	resp, err = http.Get(expiringURL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "expired playback signatures must be rejected")
	require.NoError(t, c.DeleteObject(ctx, key))
	require.NoError(t, c.DeleteObject(ctx, key), "deleting absent objects is idempotent")
	_, err = c.StatObject(ctx, key)
	require.ErrorIs(t, err, ErrObjectNotFound)
	resp, err = http.Get(url)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "old playback URL cannot access a deleted object")
}
