package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultServerPort       = "8080"
	defaultDBHost           = "localhost"
	defaultDBPort           = "5432"
	defaultDBUser           = "tokmall"
	defaultDBPassword       = "tokmall"
	defaultDBDatabase       = "tokmall"
	defaultDBSSLMode        = "disable"
	defaultMinIOEndpoint    = "localhost:9000"
	defaultMinIOBucket      = "tokmall"
	defaultUploadExpiry     = 15 * time.Minute
	defaultDownloadExpiry   = 2 * time.Hour
	defaultAvatarExpiry     = 7 * 24 * time.Hour
	defaultCursorSecret     = "change-me-cursor-secret"
	defaultRedisAddr        = "localhost:6380"
	defaultRedisDB          = 0
	defaultFeedCacheTTL     = 60 * time.Second
	defaultFeedCacheOn      = true
	defaultFollowingCacheOn = true
	defaultRankStrategy     = "time"
)

type Config struct {
	ServerPort   string
	JWTSecret    string
	CursorSecret string
	DBConfig
	MinIOConfig
	RedisConfig
	// FeedRankStrategy 关注 Feed 排序策略：time（默认，保持现状）| hot（播放+点赞热度）。
	// 切换 hot 会使 rank_version 变为 hot-v1，旧 time-v1 游标失效（客户端重新拉第一页）。
	FeedRankStrategy string
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	// FeedCacheTTL 是 feed 首页缓存的过期时间：TTL 兜底防删缓存失败导致
	// 脏数据永久存活（先写库再删缓存的 Cache-Aside 策略依赖它）。
	FeedCacheTTL time.Duration
	// FeedCacheEnabled 是缓存开关：A/B 对照压测（同样数据量下
	// 有缓存/无缓存）需要它，线上误配置关闭只会损失性能不失正确性。
	FeedCacheEnabled bool
	// FollowingCacheEnabled 关注 Feed 第一页缓存开关（同样用于 A/B 对照压测）。
	FollowingCacheEnabled bool
}

type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	SSLMode  string
}

type MinIOConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool

	UploadURLExpiry   time.Duration
	DownloadURLExpiry time.Duration
	// AvatarURLExpiry 头像签名有效期：头像出现在 feed/列表的几十个条目里，
	// 短有效期会导致批量过期重拉，故独立给长有效期（默认 7d）。
	AvatarURLExpiry time.Duration
}

func Load() (*Config, error) {
	jwtsecret, err := requiredEnv("JWT_SECRET")
	if err != nil {
		return nil, err
	}
	return &Config{
		ServerPort:   envString("SERVER_PORT", defaultServerPort),
		JWTSecret:    jwtsecret,
		CursorSecret: envString("FEED_CURSOR_SECRET", defaultCursorSecret),
		DBConfig: DBConfig{
			Host:     envString("DB_HOST", defaultDBHost),
			Port:     envString("DB_PORT", defaultDBPort),
			User:     envString("DB_USER", defaultDBUser),
			Password: envString("DB_PASSWORD", defaultDBPassword),
			Database: envString("DB_DATABASE", defaultDBDatabase),
			SSLMode:  envString("DB_SSL_MODE", defaultDBSSLMode),
		},
		MinIOConfig: MinIOConfig{
			Endpoint:          envString("MINIO_ENDPOINT", defaultMinIOEndpoint),
			AccessKey:         envString("MINIO_ACCESS_KEY", "tokmall"),
			SecretKey:         envString("MINIO_SECRET_KEY", "tokmall123"),
			Bucket:            envString("MINIO_BUCKET", defaultMinIOBucket),
			UseSSL:            envString("MINIO_USE_SSL", "") == "true",
			UploadURLExpiry:   envDuration("MINIO_UPLOAD_EXPIRY", defaultUploadExpiry),
			DownloadURLExpiry: envDuration("MINIO_DOWNLOAD_EXPIRY", defaultDownloadExpiry),
			AvatarURLExpiry:   envDuration("MINIO_AVATAR_EXPIRY", defaultAvatarExpiry),
		},
		RedisConfig: RedisConfig{
			Addr:                  envString("REDIS_ADDR", defaultRedisAddr),
			Password:              envString("REDIS_PASSWORD", ""),
			DB:                    envInt("REDIS_DB", defaultRedisDB),
			FeedCacheTTL:          envDuration("FEED_CACHE_TTL", defaultFeedCacheTTL),
			FeedCacheEnabled:      envBool("FEED_CACHE_ENABLED", defaultFeedCacheOn),
			FollowingCacheEnabled: envBool("FEED_FOLLOWING_CACHE_ENABLED", defaultFollowingCacheOn),
		},
		FeedRankStrategy: envString("FEED_RANK_STRATEGY", defaultRankStrategy),
	}, nil
}

func requiredEnv(key string) (string, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return "", fmt.Errorf("environment variable %s is required", key)
	}
	return value, nil
}

func envString(key, fallback string) string {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	value := envString(key, "")
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := envString(key, "")
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}

func envBool(key string, fallback bool) bool {
	value := envString(key, "")
	if value == "" {
		return fallback
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return b
}
