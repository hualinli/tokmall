package user

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

type userSQLLogger struct {
	logger.Interface
	queries []string
}

func (l *userSQLLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	l.queries = append(l.queries, sql)
}

func dryRunUserRepo(t *testing.T, log *userSQLLogger) *GormRepository {
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

func TestFollowCountsUsesActiveFolloweeAndFollowerReadModels(t *testing.T) {
	log := &userSQLLogger{Interface: logger.Discard}
	repo := dryRunUserRepo(t, log)

	following, followers, err := repo.FollowCounts(context.Background(), 123)
	require.ErrorIs(t, err, gorm.ErrDryRunModeUnsupported)
	require.Zero(t, following)
	require.Zero(t, followers)
	require.NotEmpty(t, log.queries)

	normalized := strings.ToLower(log.queries[len(log.queries)-1])
	require.Equal(t, 2, strings.Count(normalized, "deleted_at is null"),
		"following/followers 两个子查询都必须过滤软删用户")
	require.Equal(t, 2, strings.Count(normalized, "status = 'active'"),
		"following/followers 两个子查询都必须过滤非 ACTIVE 用户")
	require.Contains(t, normalized, "join users")
}

func TestIsFollowingFiltersInactiveFollowee(t *testing.T) {
	log := &userSQLLogger{Interface: logger.Discard}
	repo := dryRunUserRepo(t, log)

	following, err := repo.IsFollowing(context.Background(), 1, 2)
	require.NoError(t, err)
	require.False(t, following)
	require.NotEmpty(t, log.queries)

	normalized := strings.ToLower(log.queries[len(log.queries)-1])
	require.Contains(t, normalized, "join users")
	require.Contains(t, normalized, "followee_id")
	require.Contains(t, normalized, "deleted_at is null")
	require.Contains(t, normalized, "status = 'active'")
}
