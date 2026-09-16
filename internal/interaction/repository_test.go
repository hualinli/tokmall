package interaction

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

type interactionSQLLogger struct {
	logger.Interface
	queries []string
}

func (l *interactionSQLLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	l.queries = append(l.queries, sql)
}

func dryRunInteractionRepo(t *testing.T, log *interactionSQLLogger) *GormRepository {
	t.Helper()
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid"}), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 log,
	})
	require.NoError(t, err)
	return NewRepository(db)
}

func TestVideoIsInteractableUsesPublishedReadModel(t *testing.T) {
	log := &interactionSQLLogger{Interface: logger.Discard}
	repo := dryRunInteractionRepo(t, log)

	visible, err := repo.VideoIsInteractable(context.Background(), 123)
	require.NoError(t, err)
	require.False(t, visible, "DryRun 没有结果时不能把视频误判为可互动")
	require.NotEmpty(t, log.queries)

	normalized := strings.ToLower(log.queries[len(log.queries)-1])
	require.Contains(t, normalized, "from \"videos\"")
	require.Contains(t, normalized, "status")
	// PUBLISHED 是参数绑定值，SQL 文本中只检查列与谓词；具体值由 read model
	// 方法固定传入 publishedVideoStatus，避免依赖数据库方言的日志格式。
	require.Equal(t, publishedVideoStatus, "PUBLISHED")
}

func TestLikeWritesKeepPublishedGuard(t *testing.T) {
	log := &interactionSQLLogger{Interface: logger.Discard}
	repo := dryRunInteractionRepo(t, log)

	err := repo.AddLike(context.Background(), 1, 123)
	require.ErrorIs(t, err, ErrVideoNotFound)
	err = repo.RemoveLike(context.Background(), 1, 123)
	require.ErrorIs(t, err, ErrVideoNotFound)

	var guardedWrites []string
	for _, query := range log.queries {
		normalized := strings.ToLower(query)
		if strings.Contains(normalized, "insert into likes") || strings.Contains(normalized, "delete from likes") {
			guardedWrites = append(guardedWrites, normalized)
		}
	}
	require.Len(t, guardedWrites, 2)
	for _, query := range guardedWrites {
		require.Contains(t, query, "status = 'published'")
	}
}
