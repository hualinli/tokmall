package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultServerPort = "8080"
	defaultDBHost     = "localhost"
	defaultDBPort     = "5432"
	defaultDBUser     = "tokmall"
	defaultDBPassword = "tokmall"
	defaultDBDatabase = "tokmall"
	defaultDBSSLMode  = "disable"
)

type Config struct {
	ServerPort string
	JWTSecret  string
	DBConfig
}

type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	SSLMode  string
}

func Load() (*Config, error) {
	jwtsecret, err := requiredEnv("JWT_SECRET")
	if err != nil {
		return nil, err
	}
	return &Config{
		ServerPort: envString("SERVER_PORT", defaultServerPort),
		JWTSecret:  jwtsecret,
		DBConfig: DBConfig{
			Host:     envString("DB_HOST", defaultDBHost),
			Port:     envString("DB_PORT", defaultDBPort),
			User:     envString("DB_USER", defaultDBUser),
			Password: envString("DB_PASSWORD", defaultDBPassword),
			Database: envString("DB_DATABASE", defaultDBDatabase),
			SSLMode:  envString("DB_SSL_MODE", defaultDBSSLMode),
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
