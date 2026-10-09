package storage

import (
	"log/slog"
	"net/http"
	"strconv"

	"context"
	"errors"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/lihualin/tokmall/internal/config"
)

var ErrObjectNotFound = errors.New("object not found")
var ErrInvalidObjectSize = errors.New("invalid object size")

type Client struct {
	mc             *minio.Client
	bucket         string
	uploadExpiry   time.Duration
	downloadExpiry time.Duration
	avatarExpiry   time.Duration
}

func New(cfg config.S3Config) (*Client, error) {
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("init S3 client: %w", err)
	}
	c := &Client{
		mc:             mc,
		bucket:         cfg.Bucket,
		uploadExpiry:   cfg.UploadURLExpiry,
		downloadExpiry: cfg.DownloadURLExpiry,
		avatarExpiry:   cfg.AvatarURLExpiry,
	}
	// bucket 检查**不作为启动门槛**：对象存储暂时不可用不应该让整个服务起不来
	// （那会把一次依赖故障放大成全站不可用）。这里只告警并继续启动，
	// 后续调用由熔断器保护、由健康检查暴露；bucket 会在下次启动或人工创建时补齐。
	if err := c.ensureBucket(context.Background()); err != nil {
		slog.Warn("object storage is not ready at startup; continuing with degraded storage",
			"bucket", cfg.Bucket, "err", err)
	}
	return c, nil
}

// ensureBucket 启动时确保 bucket 存在。
func (c *Client) ensureBucket(ctx context.Context) error {
	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		if err := c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("create bucket: %w", err)
		}
	}
	return nil
}

// PresignPutURL 生成指定 object 的预签名 PUT URL，客户端可直传。
func (c *Client) PresignPutURL(ctx context.Context, objectKey string, fileSize int64, contentType string) (string, error) {
	if fileSize <= 0 {
		return "", ErrInvalidObjectSize
	}
	headers := http.Header{}
	headers.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	headers.Set("Content-Type", contentType)
	url, err := c.mc.PresignHeader(ctx, http.MethodPut, c.bucket, objectKey, c.uploadExpiry, nil, headers)
	if err != nil {
		return "", fmt.Errorf("presign put url: %w", err)
	}
	return url.String(), nil
}

// FinalizeUpload copies a bounded, stable snapshot into a server-only object.
// The source ETag condition prevents a concurrent PUT from changing the bytes
// between the size check and copy. Callers serialize finalization by ticket.
func (c *Client) FinalizeUpload(ctx context.Context, source, destination string, maxSize int64) error {
	info, err := c.mc.StatObject(ctx, c.bucket, source, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return ErrObjectNotFound
		}
		return fmt.Errorf("stat upload: %w", err)
	}
	if info.Size <= 0 || info.Size > maxSize {
		return ErrInvalidObjectSize
	}
	_, err = c.mc.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: c.bucket, Object: destination},
		minio.CopySrcOptions{Bucket: c.bucket, Object: source, MatchETag: info.ETag})
	if err != nil {
		return fmt.Errorf("seal upload: %w", err)
	}
	return nil
}

// PresignGetURL 生成短时效的预签名 GET URL：播放/封面的唯一出口。
// 签名即授权：删除对象后，已发出的 URL 返回 404；存储故障时，
// 物理删除由持久化清理任务补偿，旧 URL 在补删前仍可能有效。
// bucket 保持私有（默认），不再依赖"公开读"这种无法撤销的假设。
func (c *Client) PresignGetURL(ctx context.Context, objectKey string) (string, error) {
	return c.PresignGetURLWithExpiry(ctx, objectKey, c.downloadExpiry)
}

// PresignAvatarURL 头像签名：独立的长有效期（7d）。
// 头像出现在 feed/列表的几十个条目里，短有效期会批量过期，
// 前端被迫频繁重拉；头像非核心内容，长有效期可接受。
func (c *Client) PresignAvatarURL(ctx context.Context, objectKey string) (string, error) {
	return c.PresignGetURLWithExpiry(ctx, objectKey, c.avatarExpiry)
}

func (c *Client) PresignGetURLWithExpiry(ctx context.Context, objectKey string, expiry time.Duration) (string, error) {
	url, err := c.mc.PresignedGetObject(ctx, c.bucket, objectKey, expiry, nil)
	if err != nil {
		return "", fmt.Errorf("presign get url: %w", err)
	}
	return url.String(), nil
}

// StatObject 检查 object 是否存在并返回元信息（大小等）。
// 对象不存在时返回 ErrObjectNotFound（NoSuchKey 的翻译），其他错误原样上抛。
func (c *Client) StatObject(ctx context.Context, objectKey string) (int64, error) {
	info, err := c.mc.StatObject(ctx, c.bucket, objectKey, minio.StatObjectOptions{})
	if err != nil {
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return 0, ErrObjectNotFound
		}
		return 0, err
	}
	return info.Size, nil
}

// DeleteObject 删除 object。
func (c *Client) DeleteObject(ctx context.Context, objectKey string) error {
	return c.mc.RemoveObject(ctx, c.bucket, objectKey, minio.RemoveObjectOptions{})
}

// DownloadToFile 把对象下载到本地文件（媒体校验 worker 用：ffprobe 需要本地路径）。
//
// 走 FGetObject 而不是预签名 GET：worker 在服务端、有凭证，
// 不需要绕一圈 HTTP 签名，也避免把内部对象暴露成 URL。
func (c *Client) DownloadToFile(ctx context.Context, objectKey, path string) error {
	size, err := c.StatObject(ctx, objectKey)
	if err != nil {
		return err
	}
	if size <= 0 || size > 512<<20 {
		return ErrInvalidObjectSize
	}
	if err := c.mc.FGetObject(ctx, c.bucket, objectKey, path, minio.GetObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return ErrObjectNotFound
		}
		return fmt.Errorf("download object %s: %w", objectKey, err)
	}
	return nil
}

// UploadFromFile 用本地文件覆盖对象（faststart remux 后回写）。
func (c *Client) UploadFromFile(ctx context.Context, objectKey, path string, size int64) error {
	_, err := c.mc.FPutObject(ctx, c.bucket, objectKey, path, minio.PutObjectOptions{
		ContentType: "video/mp4",
	})
	if err != nil {
		return fmt.Errorf("upload object %s: %w", objectKey, err)
	}
	return nil
}
