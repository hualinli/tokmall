package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lihualin/tokmall/internal/config"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrObjectNotFound = errors.New("object not found")

type Client struct {
	mc             *minio.Client
	bucket         string
	uploadExpiry   time.Duration
	downloadExpiry time.Duration
}

func New(cfg config.MinIOConfig) (*Client, error) {
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("init minio client: %w", err)
	}
	c := &Client{
		mc:             mc,
		bucket:         cfg.Bucket,
		uploadExpiry:   cfg.UploadURLExpiry,
		downloadExpiry: cfg.DownloadURLExpiry,
	}
	if err := c.ensureBucket(context.Background()); err != nil {
		return nil, err
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
func (c *Client) PresignPutURL(ctx context.Context, objectKey string) (string, error) {
	url, err := c.mc.PresignedPutObject(ctx, c.bucket, objectKey, c.uploadExpiry)
	if err != nil {
		return "", fmt.Errorf("presign put url: %w", err)
	}
	return url.String(), nil
}

// PresignGetURL 生成短时效的预签名 GET URL：播放/封面的唯一出口。
// 签名即授权——服务器在签发那一刻决定"给不给钥匙"，删除即时性由此而来：
// 对象被删后，已发出的 URL 指向空对象（404），播放自然失败。
// bucket 保持私有（默认），不再依赖"公开读"这种无法撤销的假设。
func (c *Client) PresignGetURL(ctx context.Context, objectKey string) (string, error) {
	url, err := c.mc.PresignedGetObject(ctx, c.bucket, objectKey, c.downloadExpiry, nil)
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
