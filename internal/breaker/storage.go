package breaker

import (
	"context"
	"errors"
	"time"
)

// StorageClient 是 MinIO 客户端的接口面（消费侧接口的并集）。
// 用装饰器包住它，业务代码一行都不用改：main 装配时把原始 client 换成
// 带熔断的 client 即可。
type StorageClient interface {
	PresignPutURL(ctx context.Context, objectKey string) (string, error)
	PresignGetURL(ctx context.Context, objectKey string) (string, error)
	PresignAvatarURL(ctx context.Context, objectKey string) (string, error)
	PresignGetURLWithExpiry(ctx context.Context, objectKey string, expiry time.Duration) (string, error)
	StatObject(ctx context.Context, objectKey string) (int64, error)
	DeleteObject(ctx context.Context, objectKey string) error
	// DownloadToFile / UploadFromFile：媒体校验 worker 用（下载→探测→回写）。
	DownloadToFile(ctx context.Context, objectKey, path string) error
	UploadFromFile(ctx context.Context, objectKey, path string, size int64) error
}

// Storage 是带熔断的对象存储装饰器。
//
// 为什么对象存储值得熔断：预签名与 StatObject 都是**网络调用**，
// MinIO 不可用时每个请求都要等到底层超时（秒级），而连接池会被这些
// 挂住的调用占满——一次下游故障会放大成整个服务不可用。
// 熔断后这些调用立即失败，业务侧按"取不到 URL"降级（Feed 卡片无封面、
// 详情页无视频地址），而不是整页 500。
type Storage struct {
	next    StorageClient
	breaker *Breaker
}

func NewStorage(next StorageClient, breaker *Breaker) *Storage {
	return &Storage{next: next, breaker: breaker}
}

func (s *Storage) PresignPutURL(ctx context.Context, objectKey string) (string, error) {
	return Execute(ctx, s.breaker, func(ctx context.Context) (string, error) {
		return s.next.PresignPutURL(ctx, objectKey)
	})
}

func (s *Storage) PresignGetURL(ctx context.Context, objectKey string) (string, error) {
	return Execute(ctx, s.breaker, func(ctx context.Context) (string, error) {
		return s.next.PresignGetURL(ctx, objectKey)
	})
}

func (s *Storage) PresignAvatarURL(ctx context.Context, objectKey string) (string, error) {
	return Execute(ctx, s.breaker, func(ctx context.Context) (string, error) {
		return s.next.PresignAvatarURL(ctx, objectKey)
	})
}

func (s *Storage) PresignGetURLWithExpiry(ctx context.Context, objectKey string, expiry time.Duration) (string, error) {
	return Execute(ctx, s.breaker, func(ctx context.Context) (string, error) {
		return s.next.PresignGetURLWithExpiry(ctx, objectKey, expiry)
	})
}

func (s *Storage) StatObject(ctx context.Context, objectKey string) (int64, error) {
	return Execute(ctx, s.breaker, func(ctx context.Context) (int64, error) {
		return s.next.StatObject(ctx, objectKey)
	})
}

func (s *Storage) DeleteObject(ctx context.Context, objectKey string) error {
	return s.breaker.Do(ctx, func(ctx context.Context) error {
		return s.next.DeleteObject(ctx, objectKey)
	})
}

// DownloadToFile / UploadFromFile 同样受熔断保护：媒体校验 worker 批量下载时
// 如果对象存储已经不可用，快速失败比每个文件都等一次超时好得多。
func (s *Storage) DownloadToFile(ctx context.Context, objectKey, path string) error {
	return s.breaker.Do(ctx, func(ctx context.Context) error {
		return s.next.DownloadToFile(ctx, objectKey, path)
	})
}

func (s *Storage) UploadFromFile(ctx context.Context, objectKey, path string, size int64) error {
	return s.breaker.Do(ctx, func(ctx context.Context) error {
		return s.next.UploadFromFile(ctx, objectKey, path, size)
	})
}

// IsOpen 供健康检查/排障读取当前状态。
func (s *Storage) IsOpen() bool { return s.breaker.State() == StateOpen }

// ErrStorageUnavailable 是给业务层判断的哨兵：熔断打开时返回它，
// 让上层能区分"对象存储暂时不可用"（可降级）与其他错误。
var ErrStorageUnavailable = ErrOpen

var _ StorageClient = (*Storage)(nil)
var _ = errors.Is
