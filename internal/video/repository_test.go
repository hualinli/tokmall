package video

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// captureSQLLogger 让 repository 的 DryRun 测试可以检查最终生成的 WHERE
// 条件，而不需要启动 PostgreSQL。真正的 RowsAffected 语义由 service 的
// fake/integration 测试覆盖；这里专门防止以后改查询时漏掉 author_id。
type captureSQLLogger struct {
	logger.Interface
	sql string
}

func (l *captureSQLLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	l.sql, _ = fc()
}

func TestGormRepositoryUpdateVideoStatusIncludesAuthorPredicate(t *testing.T) {
	log := &captureSQLLogger{Interface: logger.Discard}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 log,
	})
	require.NoError(t, err)

	repo := NewRepository(db)
	now := time.Now()
	err = repo.UpdateVideoStatus(context.Background(), 123, 42, StatusPendingReview, StatusPublished, &now)
	require.ErrorIs(t, err, ErrStatusConflict, "DryRun 没有受影响行，repository 应保留 CAS 冲突语义")

	normalized := strings.ToLower(log.sql)
	require.Contains(t, normalized, "where")
	require.Contains(t, normalized, "author_id",
		"发布状态更新必须把作者归属放进原子 WHERE 条件")
	require.Contains(t, normalized, "status")
	require.Contains(t, normalized, "id")
}
