package database_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/database"
	"github.com/lihualin/tokmall/internal/testsupport"
)

func TestConsumerPoolWorksWhileForegroundIsExhausted(t *testing.T) {
	parsed, err := pgx.ParseConfig(testsupport.RequireDSN(t))
	require.NoError(t, err)
	sslMode := "disable"
	if parsed.TLSConfig != nil {
		sslMode = "require"
	}
	cfg := config.DBConfig{Host: parsed.Host, Port: strconv.Itoa(int(parsed.Port)), User: parsed.User, Password: parsed.Password, Database: parsed.Database, SSLMode: sslMode}
	// 缩小预算以免并行集成测试挤满测试服务器，验证同样的隔离性质。
	fg, err := database.ConnectWithPool(cfg, 2, 1)
	require.NoError(t, err)
	background, err := database.ConnectWithPool(cfg, 1, 1)
	require.NoError(t, err)
	fgSQL, err := fg.DB()
	require.NoError(t, err)
	bgSQL, err := background.DB()
	require.NoError(t, err)
	var held []*sql.Conn
	t.Cleanup(func() {
		for _, conn := range held {
			_ = conn.Close()
		}
		_ = fgSQL.Close()
		_ = bgSQL.Close()
	})
	for range 2 {
		conn, err := fgSQL.Conn(context.Background())
		require.NoError(t, err)
		held = append(held, conn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, fgSQL.PingContext(ctx), context.DeadlineExceeded)
	ctx, cancelBackground := context.WithTimeout(context.Background(), time.Second)
	defer cancelBackground()
	require.NoError(t, bgSQL.PingContext(ctx), "前台池耗尽不能阻止消费者取自己的连接")
	require.Equal(t, 2, fgSQL.Stats().MaxOpenConnections)
	require.Equal(t, 1, bgSQL.Stats().MaxOpenConnections)
}
