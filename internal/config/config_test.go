package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadConfigSuccess(t *testing.T) {
	clearConfigEnv(t)
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
	require.NotNil(t, cfg)
	require.Equal(t, cfg.ServerPort, "9999")
	require.Equal(t, cfg.JWTSecret, "TEST_JWT_SECRET")
	require.Equal(t, cfg.DBConfig.Host, "127.0.0.1")
	require.Equal(t, cfg.DBConfig.Port, "9990")
	require.Equal(t, cfg.DBConfig.User, "testuser")
	require.Equal(t, cfg.DBConfig.Password, "test")
	require.Equal(t, cfg.DBConfig.Database, "tbase")
	require.Equal(t, cfg.DBConfig.SSLMode, "full")
}

func TestReadConfigNotSetRequiredEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := Load()
	require.Error(t, err)
	require.Nil(t, cfg)
}

func clearConfigEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"SERVER_PORT",
		"JWT_SECRET",
		"DB_HOST",
		"DB_PORT",
		"DB_USER",
		"DB_PASSWORD",
		"DB_DATABASE",
		"DB_SSL_MODE",
	} {
		t.Setenv(key, "")
	}
}
