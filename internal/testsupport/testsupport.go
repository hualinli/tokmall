// Package testsupport 提供跨模块集成测试的共享脚手架。
//
// 集成测试一律显式开启（环境变量），并且每个测试用独立的 PostgreSQL schema：
// 这样并发跑多个包时不会互相看到对方的表，也不需要在结束后清理业务数据。
package testsupport

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	PostgresSwitch = "TOKMALL_RUN_PG_INTEGRATION"
	// RedisSwitch 单独开关：Redis 相关的证据测试（Outbox 投递、消费组）
	// 不依赖 PostgreSQL 的开关状态。
	RedisSwitch      = "TOKMALL_RUN_REDIS_INTEGRATION"
	RedisAddrEnv     = "TOKMALL_TEST_REDIS_ADDR"
	DefaultRedisAddr = "127.0.0.1:6379"
)

// RequireDSN 返回集成测试用的 PostgreSQL DSN，未开启时跳过测试。
func RequireDSN(t *testing.T) string {
	t.Helper()
	if os.Getenv(PostgresSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn, "integration switch is on, TOKMALL_TEST_DATABASE_DSN is required")
	return dsn
}

// RedisAddr 返回集成测试用的 Redis 地址，未开启时跳过测试。
func RedisAddr(t *testing.T) string {
	t.Helper()
	if os.Getenv(RedisSwitch) != "1" {
		t.Skip("set TOKMALL_RUN_REDIS_INTEGRATION=1 to run")
	}
	addr := strings.TrimSpace(os.Getenv(RedisAddrEnv))
	if addr == "" {
		addr = DefaultRedisAddr
	}
	return addr
}

// IsolatedPostgres 在目标库里建一个随机 schema、执行全部 up migration，
// 测试结束时 DROP SCHEMA CASCADE。返回的 *gorm.DB 已绑定该 search_path。
func IsolatedPostgres(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := "tokmall_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)

	scopedDSN, err := dsnWithSearchPath(dsn, schema)
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(scopedDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// 连接池上限刻意压小：`go test ./...` 会**并行**跑多个包的集成测试，
	// 每个包一个隔离 schema + 一个池。若每池开 50 条连接，5 个包就能把
	// PostgreSQL 默认的 max_connections=100 打满，报出
	// `FATAL: sorry, too many clients already (SQLSTATE 53300)`——
	// 实测踩到过：并发领券用例因此偶发失败，看起来像业务 bug，其实是测试基建
	// 把数据库连接耗尽了。并发用例仍然有效：goroutine 会在池上排队，
	// 真正并发的请求数等于池大小。
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)

	t.Cleanup(func() {
		_ = sqlDB.Close()
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		adminSQL, err := admin.DB()
		if err == nil {
			_ = adminSQL.Close()
		}
	})
	ApplyMigrations(t, db)
	return db
}

func dsnWithSearchPath(dsn, schema string) (string, error) {
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return dsn + " search_path=" + schema, nil
}

// ApplyMigrations 按文件名顺序执行 migrations/*.up.sql。
// 它不写 schema_migrations：测试关心的是"最终 schema 长什么样"，
// 而不是迁移版本表本身（那是 database.Migrate 的职责）。
func ApplyMigrations(t *testing.T, db *gorm.DB) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	sort.Strings(paths)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoErrorf(t, db.Exec(string(body)).Error, "apply %s", filepath.Base(path))
	}
}
