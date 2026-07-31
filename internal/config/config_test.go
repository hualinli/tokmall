package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadConfigSuccess(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("SERVER_PORT", "9999")
	t.Setenv("JWT_SECRET", "TEST_JWT_SECRET")
	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, cfg.ServerPort, "9999")
	require.Equal(t, cfg.JWTSecret, "TEST_JWT_SECRET")
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
	} {
		t.Setenv(key, "")
	}
}
