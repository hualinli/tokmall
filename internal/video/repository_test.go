package video

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	sql     string
	queries []string
}

func (l *captureSQLLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	l.sql, _ = fc()
	l.queries = append(l.queries, l.sql)
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

func TestGormRepositoryCreatePlayEventUsesConflictThenRead(t *testing.T) {
	log := &captureSQLLogger{Interface: logger.Discard}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 log,
	})
	require.NoError(t, err)

	repo := NewRepository(db)
	result, err := repo.CreatePlayEvent(context.Background(), &PlayEvent{
		EventID:          "0191b3e1-5f0a-7f2b-9c3d-4e5f6a7b8c9d",
		UserID:           7,
		VideoID:          123,
		PlayedAt:         time.Now().UTC().Truncate(time.Microsecond),
		PlayedAtProvided: true,
	})
	// DryRun 没有真正的 INSERT/SELECT 结果；这里关心的是 repository 已经
	// 生成“冲突后读取事实”的两段 SQL，而不是伪造一条成功写入。
	require.NoError(t, err)
	require.False(t, result.Inserted)
	require.NotNil(t, result.Existing)
	require.GreaterOrEqual(t, len(log.queries), 2)

	var insertSQL, selectSQL string
	for _, query := range log.queries {
		normalized := strings.ToLower(query)
		if strings.Contains(normalized, "insert into \"play_events\"") {
			insertSQL = normalized
		}
		if strings.Contains(normalized, "select") && strings.Contains(normalized, "from \"play_events\"") {
			selectSQL = normalized
		}
	}
	require.Contains(t, insertSQL, "on conflict")
	require.Contains(t, insertSQL, "event_id")
	require.Contains(t, selectSQL, "where event_id")
}

func TestTranslatePlayEventErrors(t *testing.T) {
	require.ErrorIs(t, translatePlayEventError(gorm.ErrRecordNotFound), ErrPlayEventNotFound)
	require.ErrorIs(t, translatePlayEventError(&pgconn.PgError{Code: "23503"}), ErrPlayEventReference)
	underlying := gorm.ErrInvalidData
	require.ErrorIs(t, translatePlayEventError(underlying), underlying)
}
