package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultServerPort     = "8080"
	defaultDBHost         = "localhost"
	defaultDBPort         = "5432"
	defaultDBUser         = "tokmall"
	defaultDBPassword     = "tokmall"
	defaultDBDatabase     = "tokmall"
	defaultDBSSLMode      = "disable"
	defaultMinIOEndpoint  = "localhost:9000"
	defaultMinIOBucket    = "tokmall"
	defaultMinIOPublicURL = "http://localhost:9000"
	defaultUploadExpiry   = 15 * time.Minute
	defaultCursorSecret   = "change-me-cursor-secret"
)

type Config struct {
	ServerPort   string
	JWTSecret    string
	CursorSecret string
	DBConfig
	MinIOConfig
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
	PublicURL string
	UseSSL    bool

	UploadURLExpiry time.Duration
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
			Endpoint:        envString("MINIO_ENDPOINT", defaultMinIOEndpoint),
			AccessKey:       envString("MINIO_ACCESS_KEY", "tokmall"),
			SecretKey:       envString("MINIO_SECRET_KEY", "tokmall123"),
			Bucket:          envString("MINIO_BUCKET", defaultMinIOBucket),
			PublicURL:       envString("MINIO_PUBLIC_URL", defaultMinIOPublicURL),
			UseSSL:          envString("MINIO_USE_SSL", "") == "true",
			UploadURLExpiry: envDuration("MINIO_UPLOAD_EXPIRY", defaultUploadExpiry),
		},
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
