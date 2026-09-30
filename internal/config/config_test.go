package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadDevelopmentUsesLocalDefaults(t *testing.T) {
	setDevelopmentEnv(t)
	t.Setenv("JWT_SECRET", "dev-jwt-secret-for-local-run")
	t.Setenv("FEED_CURSOR_SECRET", "dev-cursor-secret-for-local-run")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, EnvironmentDevelopment, cfg.Environment)
	require.Equal(t, "dev-jwt-secret-for-local-run", cfg.JWTSecret)
	require.Equal(t, "dev-cursor-secret-for-local-run", cfg.CursorSecret)
	require.Equal(t, "localhost", cfg.DBConfig.Host)
	require.False(t, cfg.MinIOConfig.UseSSL)
	require.True(t, cfg.OrderExpiryWorkerEnabled)
	require.Equal(t, 5*time.Second, cfg.OrderExpiryWorkerInterval)
	require.Equal(t, 100, cfg.OrderExpiryWorkerBatchSize)
	require.Equal(t, defaultMetricsAddr, cfg.MetricsAddr)
	require.Empty(t, cfg.TrustedProxies, "默认不信任任何转发头，限流按 TCP 对端地址")
}

// TestLoadRequiresSigningSecretsInEveryEnvironment 锁住审计结论：
// 内置开发默认密钥一旦随 APP_ENV 误配带到公网实例，任何人都能伪造 token/游标。
func TestLoadRequiresSigningSecretsInEveryEnvironment(t *testing.T) {
	t.Run("missing jwt secret", func(t *testing.T) {
		setDevelopmentEnv(t)
		t.Setenv("JWT_SECRET", "")
		_, err := Load()
		require.ErrorContains(t, err, "JWT_SECRET")
	})
	t.Run("missing cursor secret", func(t *testing.T) {
		setDevelopmentEnv(t)
		t.Setenv("FEED_CURSOR_SECRET", "")
		_, err := Load()
		require.ErrorContains(t, err, "FEED_CURSOR_SECRET")
	})
}

func TestLoadParsesTrustedProxiesAndRejectsTrustEveryone(t *testing.T) {
	setDevelopmentEnv(t)
	t.Setenv("JWT_SECRET", "dev-jwt-secret-for-local-run")
	t.Setenv("FEED_CURSOR_SECRET", "dev-cursor-secret-for-local-run")

	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 127.0.0.1/32")
	cfg, err := Load()
	require.NoError(t, err)
	require.Len(t, cfg.TrustedProxies, 2)

	t.Setenv("TRUSTED_PROXIES", "0.0.0.0/0")
	_, err = Load()
	require.ErrorContains(t, err, "must not trust every address")

	t.Setenv("TRUSTED_PROXIES", "not-a-cidr")
	_, err = Load()
	require.ErrorContains(t, err, "must be a CIDR")
}

func TestLoadValidatesMetricsAddr(t *testing.T) {
	setDevelopmentEnv(t)
	t.Setenv("JWT_SECRET", "dev-jwt-secret-for-local-run")
	t.Setenv("FEED_CURSOR_SECRET", "dev-cursor-secret-for-local-run")
	t.Setenv("METRICS_ADDR", "9091")
	_, err := Load()
	require.ErrorContains(t, err, "METRICS_ADDR")
}

func TestLoadExplicitDevelopmentConfig(t *testing.T) {
	setDevelopmentEnv(t)
	t.Setenv("SERVER_PORT", "9999")
	t.Setenv("JWT_SECRET", "TEST_JWT_SECRET")
	t.Setenv("DB_HOST", "127.0.0.1")
	t.Setenv("DB_PORT", "9990")
	t.Setenv("DB_USER", "testuser")
	t.Setenv("DB_PASSWORD", "test")
	t.Setenv("DB_DATABASE", "tbase")
	t.Setenv("DB_SSL_MODE", "full")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "9999", cfg.ServerPort)
	require.Equal(t, "TEST_JWT_SECRET", cfg.JWTSecret)
	require.Equal(t, "127.0.0.1", cfg.DBConfig.Host)
	require.Equal(t, "9990", cfg.DBConfig.Port)
	require.Equal(t, "testuser", cfg.DBConfig.User)
	require.Equal(t, "test", cfg.DBConfig.Password)
	require.Equal(t, "tbase", cfg.DBConfig.Database)
	require.Equal(t, "full", cfg.DBConfig.SSLMode)
	require.Equal(t, "time", cfg.FeedRankStrategy)
}

func TestLoadProductionAcceptsExplicitSecureConfig(t *testing.T) {
	setValidProductionEnv(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, EnvironmentProduction, cfg.Environment)
	require.True(t, cfg.MinIOConfig.UseSSL)
	require.True(t, cfg.RedisConfig.UseTLS)
}

func TestLoadProductionRequiresEveryCriticalSetting(t *testing.T) {
	setValidProductionEnv(t)
	t.Setenv("REDIS_PASSWORD", "")
	cfg, err := Load()
	require.Nil(t, cfg)
	require.EqualError(t, err, "production configuration invalid: environment variable REDIS_PASSWORD is required")
}

func TestLoadProductionRejectsWeakSecretsWithoutEchoingThem(t *testing.T) {
	setValidProductionEnv(t)
	weak := "super-private-but-too-short"
	t.Setenv("JWT_SECRET", weak)
	cfg, err := Load()
	require.Nil(t, cfg)
	require.Error(t, err)
	require.NotContains(t, err.Error(), weak)
}

func TestLoadProductionRejectsExamplePlaceholders(t *testing.T) {
	setValidProductionEnv(t)
	t.Setenv("DB_PASSWORD", "replace-me")
	_, err := Load()
	require.EqualError(t, err, "production configuration invalid: development or placeholder credentials are not allowed")
}

func TestLoadProductionRejectsSharedSigningSecrets(t *testing.T) {
	setValidProductionEnv(t)
	shared := strings.Repeat("a", 40)
	t.Setenv("JWT_SECRET", shared)
	t.Setenv("FEED_CURSOR_SECRET", shared)
	_, err := Load()
	require.EqualError(t, err, "production configuration invalid: JWT_SECRET and FEED_CURSOR_SECRET must be different")
}

func TestLoadProductionRejectsInsecureTransports(t *testing.T) {
	t.Run("database TLS disabled", func(t *testing.T) {
		setValidProductionEnv(t)
		t.Setenv("DB_SSL_MODE", "disable")
		_, err := Load()
		require.EqualError(t, err, "production configuration invalid: DB_SSL_MODE must enable TLS")
	})
	t.Run("object storage TLS disabled", func(t *testing.T) {
		setValidProductionEnv(t)
		t.Setenv("MINIO_USE_SSL", "false")
		_, err := Load()
		require.EqualError(t, err, "production configuration invalid: MINIO_USE_SSL must be true")
	})
	t.Run("redis TLS disabled", func(t *testing.T) {
		setValidProductionEnv(t)
		t.Setenv("REDIS_USE_TLS", "false")
		_, err := Load()
		require.EqualError(t, err, "production configuration invalid: REDIS_USE_TLS must be true")
	})
}

func TestLoadRejectsExplicitMalformedValuesInEveryEnvironment(t *testing.T) {
	tests := []struct{ key, value, want string }{
		{"REDIS_DB", "one", "environment variable REDIS_DB must be an integer"},
		{"FEED_CACHE_TTL", "soon", "environment variable FEED_CACHE_TTL must be a valid duration"},
		{"FEED_CACHE_ENABLED", "sometimes", "environment variable FEED_CACHE_ENABLED must be a boolean"},
		{"MINIO_USE_SSL", "yes-please", "environment variable MINIO_USE_SSL must be a boolean"},
		{"ORDER_EXPIRY_WORKER_ENABLED", "sometimes", "environment variable ORDER_EXPIRY_WORKER_ENABLED must be a boolean"},
		{"ORDER_EXPIRY_WORKER_INTERVAL", "soon", "environment variable ORDER_EXPIRY_WORKER_INTERVAL must be a valid duration"},
		{"ORDER_EXPIRY_WORKER_BATCH_SIZE", "many", "environment variable ORDER_EXPIRY_WORKER_BATCH_SIZE must be an integer"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			setDevelopmentEnv(t)
			t.Setenv(tt.key, tt.value)
			_, err := Load()
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestLoadRejectsNonPositiveOrderExpiryWorkerConfig(t *testing.T) {
	for _, tt := range []struct{ key, value, want string }{
		{"ORDER_EXPIRY_WORKER_INTERVAL", "0s", "ORDER_EXPIRY_WORKER_INTERVAL must be positive"},
		{"ORDER_EXPIRY_WORKER_BATCH_SIZE", "0", "ORDER_EXPIRY_WORKER_BATCH_SIZE must be positive"},
	} {
		t.Run(tt.key, func(t *testing.T) {
			setDevelopmentEnv(t)
			t.Setenv(tt.key, tt.value)
			_, err := Load()
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("APP_ENV", "prod")
	_, err := Load()
	require.EqualError(t, err, `APP_ENV must be "development" or "production"`)
}

func TestLoadRequiresExplicitEnvironment(t *testing.T) {
	clearConfigEnv(t)
	_, err := Load()
	require.EqualError(t, err, "environment variable APP_ENV is required")
}

func TestLoadRejectsHotRankStrategy(t *testing.T) {
	setDevelopmentEnv(t)
	t.Setenv("FEED_RANK_STRATEGY", "hot")
	cfg, err := Load()
	require.ErrorIs(t, err, ErrInvalidFeedRankStrategy)
	require.Nil(t, cfg)
}

func TestLoadRejectsUnknownRankStrategy(t *testing.T) {
	setDevelopmentEnv(t)
	t.Setenv("FEED_RANK_STRATEGY", "typo")
	cfg, err := Load()
	require.ErrorIs(t, err, ErrInvalidFeedRankStrategy)
	require.Nil(t, cfg)
}

func setValidProductionEnv(t *testing.T) {
	t.Helper()
	clearConfigEnv(t)
	values := map[string]string{
		"APP_ENV": EnvironmentProduction, "JWT_SECRET": strings.Repeat("j", 40),
		"FEED_CURSOR_SECRET": strings.Repeat("c", 40), "DB_HOST": "postgres.internal",
		"DB_PORT": "5432", "DB_USER": "tokmall_app", "DB_PASSWORD": "database-password",
		"DB_DATABASE": "tokmall", "DB_SSL_MODE": "verify-full", "MINIO_ENDPOINT": "objects.internal:9000",
		"MINIO_ACCESS_KEY": "object-access-key", "MINIO_SECRET_KEY": "object-secret-key",
		"MINIO_BUCKET": "tokmall-production", "MINIO_USE_SSL": "true",
		"REDIS_ADDR": "redis.internal:6379", "REDIS_PASSWORD": "redis-password", "REDIS_USE_TLS": "true",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
}

func setDevelopmentEnv(t *testing.T) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("APP_ENV", EnvironmentDevelopment)
	t.Setenv("JWT_SECRET", "dev-jwt-secret-for-local-run")
	t.Setenv("FEED_CURSOR_SECRET", "dev-cursor-secret-for-local-run")
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"APP_ENV", "SERVER_PORT", "JWT_SECRET", "FEED_CURSOR_SECRET",
		"METRICS_ADDR", "TRUSTED_PROXIES",
		"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_DATABASE", "DB_SSL_MODE",
		"MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "MINIO_BUCKET", "MINIO_USE_SSL",
		"MINIO_UPLOAD_EXPIRY", "MINIO_DOWNLOAD_EXPIRY", "MINIO_AVATAR_EXPIRY",
		"REDIS_ADDR", "REDIS_PASSWORD", "REDIS_DB", "REDIS_USE_TLS", "FEED_CACHE_TTL", "FEED_CACHE_ENABLED",
		"FEED_FOLLOWING_CACHE_ENABLED", "FEED_RANK_STRATEGY", "RATE_LIMIT_ENABLED",
		"RATE_LIMIT_AUTH_ACCOUNT", "RATE_LIMIT_AUTH_IP", "RATE_LIMIT_WRITE", "RATE_LIMIT_READ",
		"ORDER_EXPIRY_WORKER_ENABLED", "ORDER_EXPIRY_WORKER_INTERVAL", "ORDER_EXPIRY_WORKER_BATCH_SIZE",
	} {
		t.Setenv(key, "")
	}
}
