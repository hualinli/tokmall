package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvironmentDevelopment = "development"
	EnvironmentProduction  = "production"

	defaultServerPort           = "8080"
	defaultDBHost               = "localhost"
	defaultDBPort               = "5432"
	defaultDBUser               = "tokmall"
	defaultDBPassword           = "tokmall"
	defaultDBDatabase           = "tokmall"
	defaultDBSSLMode            = "disable"
	defaultMinIOEndpoint        = "localhost:9000"
	defaultMinIOBucket          = "tokmall"
	defaultUploadExpiry         = 15 * time.Minute
	defaultDownloadExpiry       = 2 * time.Hour
	defaultAvatarExpiry         = 7 * 24 * time.Hour
	defaultRedisAddr            = "localhost:6380"
	defaultRedisDB              = 0
	defaultFeedCacheTTL         = 60 * time.Second
	defaultFeedL1Enabled        = true
	defaultFeedL1TTL            = 5 * time.Second
	defaultFeedL1Capacity       = 512
	defaultFeedVersionInterval  = time.Second
	defaultHotFeedEnabled       = true
	defaultFeedCacheOn          = true
	defaultFollowingCacheOn     = true
	defaultRankStrategy         = "time"
	rateLimitOn                 = true
	defaultAuthAccountLimit     = 10   // 认证接口：10 次/分钟/账号（防暴力破解，按账号维度）
	defaultAuthIPLimit          = 100  // 认证接口：100 次/分钟/IP（防单一来源洪峰，NAT 下放宽）
	defaultWriteLimit           = 100  // 写接口：100 次/10s/用户
	defaultReadLimit            = 6000 // 读接口：6000 次/分钟/IP（只挡爬虫/洪峰，NAT 多人共享不误伤）
	defaultOrderExpiryWorkerOn  = true
	defaultOrderExpiryInterval  = 5 * time.Second
	defaultOrderExpiryBatchSize = 100
	defaultOutboxRelayOn        = true
	defaultOutboxRelayTick      = time.Second
	defaultOutboxRelayBatch     = 50
	defaultOutboxRelayLease     = 30 * time.Second
	defaultOutboxMaxAttempts    = 5
	defaultOutboxStream         = "tokmall:events:commerce.v1"
	defaultOutboxDLQSuffix      = ":dlq"
	defaultOutboxStreamMaxLen   = 10000
	defaultOutboxConsumerOn     = true
	defaultOutboxGroup          = "attribution-projection"
	defaultOutboxMaxDeliver     = 5
	defaultOutboxClaimIdle      = 30 * time.Second
	defaultReconcileOn          = true
	defaultReconcileInterval    = 5 * time.Minute
	defaultReconcileGrace       = time.Minute
	defaultExistenceOn          = true
	defaultExistenceExpected    = 1_000_000
	defaultExistenceFPRate      = 0.01
	defaultExistenceNegTTL      = 30 * time.Second
	defaultExistenceNegJitter   = 15 * time.Second
	defaultExistenceRebuildTick = 10 * time.Minute
	defaultBreakerOn            = true
	defaultBreakerThreshold     = 5
	defaultBreakerOpenDuration  = 5 * time.Second
	defaultBreakerMaxOpen       = 30 * time.Second

	// defaultMetricsAddr 默认只监听回环地址：/metrics 会暴露路由级 QPS/延迟、
	// 连接池与运行时指标，属于容量指纹。生产如需 Prometheus 抓取，
	// 应显式配置 METRICS_ADDR 到内网地址，并由网络策略限制访问。
	defaultMetricsAddr = "127.0.0.1:9091"
)

// ErrInvalidFeedRankStrategy 表示排序策略不是当前生产配置支持的策略。
// HotRanker 仍保留在 feed 包中用于排序实验，但它的排序键与当前时间游标
// 不一致，不能通过生产配置暴露为可翻页的关注 Feed。
var ErrInvalidFeedRankStrategy = errors.New("invalid feed rank strategy")

type Config struct {
	Environment  string
	ServerPort   string
	JWTSecret    string
	CursorSecret string
	// MetricsAddr 是 Prometheus 指标端口的监听地址（默认仅回环）。
	MetricsAddr string
	// TrustedProxies 是允许提供 X-Forwarded-For 的代理网段（CIDR 列表）。
	// 默认为空 = 不信任任何转发头，限流按 TCP 对端地址计算。
	// 配错（过宽）会让客户端伪造 XFF 绕过按 IP 的限流。
	TrustedProxies []*net.IPNet
	DBConfig
	MinIOConfig
	RedisConfig
	// FeedRankStrategy 关注 Feed 排序策略。当前生产只支持 time（默认）。
	// hot 仅保留为 feed.HotRanker 的单元实验，因分页语义尚未闭合而拒绝启动配置。
	FeedRankStrategy string
	// RateLimitEnabled 限流总开关：压测/调试时关闭（feed 压测 50 并发会被 IP 限流挡住）。
	RateLimitEnabled bool
	// AuthAccountLimit 认证接口按账号配额（NAT 下多人共享 IP，按账号才能精确防暴力破解）。
	AuthAccountLimit int
	// AuthIPLimit 认证接口按 IP 配额（放宽版，防单一来源洪峰）。
	AuthIPLimit int
	// WriteRateLimit 写接口按用户配额。
	WriteRateLimit int
	// ReadRateLimit 读接口按 IP 配额（高阈值：只挡爬虫/异常洪峰，不误伤真实用户）。
	ReadRateLimit            int
	OrderExpiryWorkerEnabled bool
	// Outbox 相关配置：中继把 outbox_events 投递到事件流，消费者做幂等投影。
	OutboxRelayEnabled          bool
	OutboxRelayInterval         time.Duration
	OutboxRelayBatchSize        int
	OutboxRelayLease            time.Duration
	OutboxMaxAttempts           int
	OutboxStream                string
	OutboxDLQStream             string
	OutboxStreamMaxLen          int64
	OutboxConsumerEnabled       bool
	OutboxConsumerGroup         string
	OutboxConsumerMaxDeliveries int
	OutboxClaimIdle             time.Duration
	// ReconcileEnabled：周期对账 + 对账驱动的事件重放。
	ReconcileEnabled  bool
	ReconcileInterval time.Duration
	// ReconcileGrace：事件发布后等待消费回执的宽限期，超过即视为可能丢失。
	ReconcileGrace time.Duration
	// ExistenceEnabled：缓存穿透防护（布隆过滤器 + 负缓存）。
	ExistenceEnabled bool
	// ExistenceExpected 是每个命名空间的预期元素数，决定位数组大小与误判率。
	ExistenceExpected uint64
	// ExistenceFPRate 是目标误判率（假阳性；假阴性恒为 0）。
	ExistenceFPRate float64
	// ExistenceNegativeTTL/Jitter 是负缓存有效期与其随机抖动（防集中过期）。
	ExistenceNegativeTTL    time.Duration
	ExistenceNegativeJitter time.Duration
	// ExistenceRebuildInterval 是全量重建布隆过滤器的周期（删除无法从过滤器移除，
	// 靠重建把误判率拉回设计值）。
	ExistenceRebuildInterval time.Duration
	// BreakerEnabled：对象存储等下游依赖的熔断器。
	BreakerEnabled bool
	// BreakerFailureThreshold 是连续失败多少次后打开。
	BreakerFailureThreshold int
	// BreakerOpenDuration/Max 是冷却期及其指数退避上限。
	BreakerOpenDuration        time.Duration
	BreakerMaxOpen             time.Duration
	OrderExpiryWorkerInterval  time.Duration
	OrderExpiryWorkerBatchSize int
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	UseTLS   bool
	// FeedL1Enabled/FeedL1TTL/FeedL1Capacity：进程内一级缓存（两级缓存的 L1）。
	// L1 是"最终一致"的一层：TTL 要远短于 L2，作为失效机制失灵时的兜底。
	FeedL1Enabled  bool
	FeedL1TTL      time.Duration
	FeedL1Capacity int
	// FeedVersionInterval 是版本号探测间隔：L1 失效延迟的上界（见 feed.VersionStamp）。
	FeedVersionInterval time.Duration
	// HotFeedEnabled：热榜（分钟桶 + 冻结窗口）。关闭时 /feed/hot 降级为时间流。
	HotFeedEnabled bool
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
	environment, err := requiredEnv("APP_ENV")
	if err != nil {
		return nil, err
	}
	if environment != EnvironmentDevelopment && environment != EnvironmentProduction {
		return nil, fmt.Errorf("APP_ENV must be %q or %q", EnvironmentDevelopment, EnvironmentProduction)
	}
	feedRankStrategy := envString("FEED_RANK_STRATEGY", defaultRankStrategy)
	if err := validateFeedRankStrategy(feedRankStrategy); err != nil {
		return nil, err
	}
	redisDB, err := envInt("REDIS_DB", defaultRedisDB)
	if err != nil {
		return nil, err
	}
	hotFeedEnabled, err := envBool("HOT_FEED_ENABLED", defaultHotFeedEnabled)
	if err != nil {
		return nil, err
	}
	feedL1Enabled, err := envBool("FEED_L1_ENABLED", defaultFeedL1Enabled)
	if err != nil {
		return nil, err
	}
	feedL1TTL, err := envDuration("FEED_L1_TTL", defaultFeedL1TTL)
	if err != nil {
		return nil, err
	}
	feedL1Capacity, err := envInt("FEED_L1_CAPACITY", defaultFeedL1Capacity)
	if err != nil {
		return nil, err
	}
	feedVersionInterval, err := envDuration("FEED_VERSION_INTERVAL", defaultFeedVersionInterval)
	if err != nil {
		return nil, err
	}
	feedCacheTTL, err := envDuration("FEED_CACHE_TTL", defaultFeedCacheTTL)
	if err != nil {
		return nil, err
	}
	feedCacheEnabled, err := envBool("FEED_CACHE_ENABLED", defaultFeedCacheOn)
	if err != nil {
		return nil, err
	}
	followingCacheEnabled, err := envBool("FEED_FOLLOWING_CACHE_ENABLED", defaultFollowingCacheOn)
	if err != nil {
		return nil, err
	}
	redisUseTLS, err := envBool("REDIS_USE_TLS", false)
	if err != nil {
		return nil, err
	}
	minIOUseSSL, err := envBool("MINIO_USE_SSL", false)
	if err != nil {
		return nil, err
	}
	uploadExpiry, err := envDuration("MINIO_UPLOAD_EXPIRY", defaultUploadExpiry)
	if err != nil {
		return nil, err
	}
	downloadExpiry, err := envDuration("MINIO_DOWNLOAD_EXPIRY", defaultDownloadExpiry)
	if err != nil {
		return nil, err
	}
	avatarExpiry, err := envDuration("MINIO_AVATAR_EXPIRY", defaultAvatarExpiry)
	if err != nil {
		return nil, err
	}
	rateLimitEnabled, err := envBool("RATE_LIMIT_ENABLED", rateLimitOn)
	if err != nil {
		return nil, err
	}
	authAccountLimit, err := envInt("RATE_LIMIT_AUTH_ACCOUNT", defaultAuthAccountLimit)
	if err != nil {
		return nil, err
	}
	authIPLimit, err := envInt("RATE_LIMIT_AUTH_IP", defaultAuthIPLimit)
	if err != nil {
		return nil, err
	}
	writeLimit, err := envInt("RATE_LIMIT_WRITE", defaultWriteLimit)
	if err != nil {
		return nil, err
	}
	readLimit, err := envInt("RATE_LIMIT_READ", defaultReadLimit)
	if err != nil {
		return nil, err
	}
	orderExpiryWorkerEnabled, err := envBool("ORDER_EXPIRY_WORKER_ENABLED", defaultOrderExpiryWorkerOn)
	if err != nil {
		return nil, err
	}
	orderExpiryWorkerInterval, err := envDuration("ORDER_EXPIRY_WORKER_INTERVAL", defaultOrderExpiryInterval)
	if err != nil {
		return nil, err
	}
	orderExpiryWorkerBatchSize, err := envInt("ORDER_EXPIRY_WORKER_BATCH_SIZE", defaultOrderExpiryBatchSize)
	if err != nil {
		return nil, err
	}
	outboxRelayEnabled, err := envBool("OUTBOX_RELAY_ENABLED", defaultOutboxRelayOn)
	if err != nil {
		return nil, err
	}
	outboxRelayInterval, err := envDuration("OUTBOX_RELAY_INTERVAL", defaultOutboxRelayTick)
	if err != nil {
		return nil, err
	}
	outboxRelayBatch, err := envInt("OUTBOX_RELAY_BATCH_SIZE", defaultOutboxRelayBatch)
	if err != nil {
		return nil, err
	}
	outboxRelayLease, err := envDuration("OUTBOX_RELAY_LEASE", defaultOutboxRelayLease)
	if err != nil {
		return nil, err
	}
	outboxMaxAttempts, err := envInt("OUTBOX_MAX_ATTEMPTS", defaultOutboxMaxAttempts)
	if err != nil {
		return nil, err
	}
	outboxStreamMaxLen, err := envInt64("OUTBOX_STREAM_MAX_LEN", defaultOutboxStreamMaxLen)
	if err != nil {
		return nil, err
	}
	outboxConsumerEnabled, err := envBool("OUTBOX_CONSUMER_ENABLED", defaultOutboxConsumerOn)
	if err != nil {
		return nil, err
	}
	outboxMaxDeliveries, err := envInt("OUTBOX_CONSUMER_MAX_DELIVERIES", defaultOutboxMaxDeliver)
	if err != nil {
		return nil, err
	}
	outboxClaimIdle, err := envDuration("OUTBOX_CONSUMER_CLAIM_IDLE", defaultOutboxClaimIdle)
	if err != nil {
		return nil, err
	}
	outboxStream := envString("OUTBOX_STREAM", defaultOutboxStream)
	reconcileEnabled, err := envBool("RECONCILE_ENABLED", defaultReconcileOn)
	if err != nil {
		return nil, err
	}
	reconcileInterval, err := envDuration("RECONCILE_INTERVAL", defaultReconcileInterval)
	if err != nil {
		return nil, err
	}
	reconcileGrace, err := envDuration("RECONCILE_GRACE", defaultReconcileGrace)
	if err != nil {
		return nil, err
	}
	existenceEnabled, err := envBool("EXISTENCE_GUARD_ENABLED", defaultExistenceOn)
	if err != nil {
		return nil, err
	}
	existenceExpected, err := envInt64("EXISTENCE_EXPECTED_ITEMS", defaultExistenceExpected)
	if err != nil {
		return nil, err
	}
	existenceFPRate, err := envFloat("EXISTENCE_FP_RATE", defaultExistenceFPRate)
	if err != nil {
		return nil, err
	}
	existenceNegTTL, err := envDuration("EXISTENCE_NEGATIVE_TTL", defaultExistenceNegTTL)
	if err != nil {
		return nil, err
	}
	existenceNegJitter, err := envDuration("EXISTENCE_NEGATIVE_JITTER", defaultExistenceNegJitter)
	if err != nil {
		return nil, err
	}
	existenceRebuild, err := envDuration("EXISTENCE_REBUILD_INTERVAL", defaultExistenceRebuildTick)
	if err != nil {
		return nil, err
	}
	breakerEnabled, err := envBool("BREAKER_ENABLED", defaultBreakerOn)
	if err != nil {
		return nil, err
	}
	breakerThreshold, err := envInt("BREAKER_FAILURE_THRESHOLD", defaultBreakerThreshold)
	if err != nil {
		return nil, err
	}
	breakerOpenDuration, err := envDuration("BREAKER_OPEN_DURATION", defaultBreakerOpenDuration)
	if err != nil {
		return nil, err
	}
	breakerMaxOpen, err := envDuration("BREAKER_MAX_OPEN_DURATION", defaultBreakerMaxOpen)
	if err != nil {
		return nil, err
	}
	trustedProxies, err := parseTrustedProxies(envString("TRUSTED_PROXIES", ""))
	if err != nil {
		return nil, err
	}
	// 签名密钥在**所有环境**都必须显式提供：内置开发默认值一旦随
	// APP_ENV 误配带到公网实例，等于任何人都能伪造 token 或游标。
	jwtSecret, err := requiredEnv("JWT_SECRET")
	if err != nil {
		return nil, fmt.Errorf("%w (开发环境可在 make dev 中通过 JWT_SECRET=... 传入)", err)
	}
	cursorSecret, err := requiredEnv("FEED_CURSOR_SECRET")
	if err != nil {
		return nil, fmt.Errorf("%w (开发环境可在 make dev 中通过 FEED_CURSOR_SECRET=... 传入)", err)
	}

	cfg := &Config{
		Environment:    environment,
		ServerPort:     envString("SERVER_PORT", defaultServerPort),
		JWTSecret:      jwtSecret,
		CursorSecret:   cursorSecret,
		MetricsAddr:    envString("METRICS_ADDR", defaultMetricsAddr),
		TrustedProxies: trustedProxies,
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
			UseSSL:            minIOUseSSL,
			UploadURLExpiry:   uploadExpiry,
			DownloadURLExpiry: downloadExpiry,
			AvatarURLExpiry:   avatarExpiry,
		},
		RedisConfig: RedisConfig{
			Addr:                  envString("REDIS_ADDR", defaultRedisAddr),
			Password:              envString("REDIS_PASSWORD", ""),
			DB:                    redisDB,
			UseTLS:                redisUseTLS,
			FeedCacheTTL:          feedCacheTTL,
			HotFeedEnabled:        hotFeedEnabled,
			FeedL1Enabled:         feedL1Enabled,
			FeedL1TTL:             feedL1TTL,
			FeedL1Capacity:        feedL1Capacity,
			FeedVersionInterval:   feedVersionInterval,
			FeedCacheEnabled:      feedCacheEnabled,
			FollowingCacheEnabled: followingCacheEnabled,
		},
		FeedRankStrategy:           feedRankStrategy,
		RateLimitEnabled:           rateLimitEnabled,
		AuthAccountLimit:           authAccountLimit,
		AuthIPLimit:                authIPLimit,
		WriteRateLimit:             writeLimit,
		ReadRateLimit:              readLimit,
		OrderExpiryWorkerEnabled:   orderExpiryWorkerEnabled,
		OrderExpiryWorkerInterval:  orderExpiryWorkerInterval,
		OrderExpiryWorkerBatchSize: orderExpiryWorkerBatchSize,

		OutboxRelayEnabled:          outboxRelayEnabled,
		OutboxRelayInterval:         outboxRelayInterval,
		OutboxRelayBatchSize:        outboxRelayBatch,
		OutboxRelayLease:            outboxRelayLease,
		OutboxMaxAttempts:           outboxMaxAttempts,
		OutboxStream:                outboxStream,
		OutboxDLQStream:             outboxStream + defaultOutboxDLQSuffix,
		OutboxStreamMaxLen:          outboxStreamMaxLen,
		OutboxConsumerEnabled:       outboxConsumerEnabled,
		OutboxConsumerGroup:         envString("OUTBOX_CONSUMER_GROUP", defaultOutboxGroup),
		OutboxConsumerMaxDeliveries: outboxMaxDeliveries,
		OutboxClaimIdle:             outboxClaimIdle,

		ReconcileEnabled:  reconcileEnabled,
		ReconcileInterval: reconcileInterval,
		ReconcileGrace:    reconcileGrace,

		ExistenceEnabled:         existenceEnabled,
		ExistenceExpected:        uint64(existenceExpected),
		ExistenceFPRate:          existenceFPRate,
		ExistenceNegativeTTL:     existenceNegTTL,
		ExistenceNegativeJitter:  existenceNegJitter,
		ExistenceRebuildInterval: existenceRebuild,

		BreakerEnabled:          breakerEnabled,
		BreakerFailureThreshold: breakerThreshold,
		BreakerOpenDuration:     breakerOpenDuration,
		BreakerMaxOpen:          breakerMaxOpen,
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if _, err := strconv.ParseUint(c.ServerPort, 10, 16); err != nil || c.ServerPort == "0" {
		return errors.New("SERVER_PORT must be an integer between 1 and 65535")
	}
	if _, err := strconv.ParseUint(c.Port, 10, 16); err != nil || c.Port == "0" {
		return errors.New("DB_PORT must be an integer between 1 and 65535")
	}
	if c.DB < 0 {
		return errors.New("REDIS_DB must be non-negative")
	}
	if c.FeedCacheTTL <= 0 || c.UploadURLExpiry <= 0 || c.DownloadURLExpiry <= 0 || c.AvatarURLExpiry <= 0 {
		return errors.New("cache and signed URL expiry values must be positive")
	}
	if c.RateLimitEnabled && (c.AuthAccountLimit <= 0 || c.AuthIPLimit <= 0 || c.WriteRateLimit <= 0 || c.ReadRateLimit <= 0) {
		return errors.New("rate limit values must be positive when rate limiting is enabled")
	}
	if c.OrderExpiryWorkerInterval <= 0 {
		return errors.New("ORDER_EXPIRY_WORKER_INTERVAL must be positive")
	}
	if c.OrderExpiryWorkerBatchSize <= 0 {
		return errors.New("ORDER_EXPIRY_WORKER_BATCH_SIZE must be positive")
	}
	if c.OutboxRelayInterval <= 0 || c.OutboxRelayBatchSize <= 0 || c.OutboxRelayLease <= 0 || c.OutboxMaxAttempts <= 0 {
		return errors.New("outbox relay interval, batch size, lease and max attempts must be positive")
	}
	if c.OutboxStreamMaxLen <= 0 {
		return errors.New("OUTBOX_STREAM_MAX_LEN must be positive")
	}
	if strings.TrimSpace(c.OutboxStream) == "" || strings.TrimSpace(c.OutboxConsumerGroup) == "" {
		return errors.New("OUTBOX_STREAM and OUTBOX_CONSUMER_GROUP must be non-empty")
	}
	if c.OutboxConsumerMaxDeliveries <= 0 || c.OutboxClaimIdle <= 0 {
		return errors.New("outbox consumer max deliveries and claim idle must be positive")
	}
	if c.ReconcileInterval <= 0 || c.ReconcileGrace <= 0 {
		return errors.New("RECONCILE_INTERVAL and RECONCILE_GRACE must be positive")
	}
	if c.FeedL1Enabled {
		if c.FeedL1TTL <= 0 || c.FeedL1TTL > c.FeedCacheTTL {
			// L1 比 L2 活得久没有意义：只会让陈旧窗口更长。
			return errors.New("FEED_L1_TTL must be positive and not greater than FEED_CACHE_TTL")
		}
		if c.FeedL1Capacity <= 0 || c.FeedVersionInterval <= 0 {
			return errors.New("FEED_L1_CAPACITY and FEED_VERSION_INTERVAL must be positive")
		}
	}
	if c.BreakerEnabled {
		if c.BreakerFailureThreshold <= 0 || c.BreakerOpenDuration <= 0 || c.BreakerMaxOpen <= 0 {
			return errors.New("breaker threshold and durations must be positive")
		}
		if c.BreakerMaxOpen < c.BreakerOpenDuration {
			return errors.New("BREAKER_MAX_OPEN_DURATION must not be shorter than BREAKER_OPEN_DURATION")
		}
	}
	if c.ExistenceEnabled {
		if c.ExistenceExpected == 0 {
			return errors.New("EXISTENCE_EXPECTED_ITEMS must be positive")
		}
		if c.ExistenceFPRate <= 0 || c.ExistenceFPRate >= 1 {
			return errors.New("EXISTENCE_FP_RATE must be in (0,1)")
		}
		if c.ExistenceNegativeTTL <= 0 || c.ExistenceNegativeJitter < 0 {
			return errors.New("EXISTENCE_NEGATIVE_TTL must be positive and jitter non-negative")
		}
		if c.ExistenceRebuildInterval <= 0 {
			return errors.New("EXISTENCE_REBUILD_INTERVAL must be positive")
		}
	}
	if host, port, err := net.SplitHostPort(c.MetricsAddr); err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return errors.New("METRICS_ADDR must use host:port format")
	}
	for _, cidr := range c.TrustedProxies {
		if ones, bits := cidr.Mask.Size(); bits > 0 && ones == 0 {
			// 信任 0.0.0.0/0 等于信任所有人：任何客户端都能自带 X-Forwarded-For
			// 伪造来源地址，按 IP 的限流会被逐请求绕过。
			return fmt.Errorf("TRUSTED_PROXIES must not trust every address (%s)", cidr.String())
		}
	}
	if c.Environment != EnvironmentProduction {
		return nil
	}

	required := []string{
		"JWT_SECRET", "FEED_CURSOR_SECRET",
		"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_DATABASE", "DB_SSL_MODE",
		"MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "MINIO_BUCKET", "MINIO_USE_SSL",
		"REDIS_ADDR", "REDIS_PASSWORD", "REDIS_USE_TLS",
	}
	for _, key := range required {
		if _, err := requiredEnv(key); err != nil {
			return fmt.Errorf("production configuration invalid: %w", err)
		}
	}
	if len(c.JWTSecret) < 32 {
		return errors.New("production configuration invalid: JWT_SECRET must contain at least 32 characters")
	}
	if len(c.CursorSecret) < 32 {
		return errors.New("production configuration invalid: FEED_CURSOR_SECRET must contain at least 32 characters")
	}
	if c.JWTSecret == c.CursorSecret {
		return errors.New("production configuration invalid: JWT_SECRET and FEED_CURSOR_SECRET must be different")
	}
	if isKnownDevelopmentSecret(c.JWTSecret) || isKnownDevelopmentSecret(c.CursorSecret) ||
		isKnownDevelopmentSecret(c.DBConfig.Password) || isKnownDevelopmentSecret(c.AccessKey) ||
		isKnownDevelopmentSecret(c.SecretKey) || isKnownDevelopmentSecret(c.RedisConfig.Password) ||
		c.DBConfig.Password == defaultDBPassword || c.SecretKey == "tokmall123" {
		return errors.New("production configuration invalid: development or placeholder credentials are not allowed")
	}
	if c.SSLMode == "disable" {
		return errors.New("production configuration invalid: DB_SSL_MODE must enable TLS")
	}
	if !c.UseSSL {
		return errors.New("production configuration invalid: MINIO_USE_SSL must be true")
	}
	if !c.UseTLS {
		return errors.New("production configuration invalid: REDIS_USE_TLS must be true")
	}
	if host, _, err := net.SplitHostPort(c.Endpoint); err != nil || strings.TrimSpace(host) == "" {
		return errors.New("production configuration invalid: MINIO_ENDPOINT must use host:port format")
	}
	return nil
}

func isKnownDevelopmentSecret(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(normalized, "replace-me") || strings.Contains(normalized, "changeme") {
		return true
	}
	switch normalized {
	case "test", "secret", "changeme", "change-me", "dev-only-jwt-secret", "change-me-cursor-secret":
		return true
	default:
		return false
	}
}

// parseTrustedProxies 解析逗号分隔的 CIDR 列表。空值表示不信任任何转发头。
// 这里刻意不接受裸 IP：CIDR 写法能强制配置者想清楚信任范围（/32 也比
// 隐式的"全部信任"安全）。
func parseTrustedProxies(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var cidrs []*net.IPNet
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		_, network, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES entry %q must be a CIDR such as 10.0.0.0/8", item)
		}
		cidrs = append(cidrs, network)
	}
	return cidrs, nil
}

func validateFeedRankStrategy(strategy string) error {
	if strategy != defaultRankStrategy {
		return fmt.Errorf("%w: %q is not production-supported; only %q is available", ErrInvalidFeedRankStrategy, strategy, defaultRankStrategy)
	}
	return nil
}

func requiredEnv(key string) (string, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
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

func envInt(key string, fallback int) (int, error) {
	value := envString(key, "")
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s must be an integer", key)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := envString(key, "")
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s must be a valid duration", key)
	}
	return duration, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	value := envString(key, "")
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s must be an integer", key)
	}
	return n, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	value := envString(key, "")
	if value == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s must be a number", key)
	}
	return f, nil
}

func envBool(key string, fallback bool) (bool, error) {
	value := envString(key, "")
	if value == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("environment variable %s must be a boolean", key)
	}
	return b, nil
}
