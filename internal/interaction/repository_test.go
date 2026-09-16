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

func TestUserExistsUsesActiveUserReadModel(t *testing.T) {
	log := &interactionSQLLogger{Interface: logger.Discard}
	repo := dryRunInteractionRepo(t, log)

	exists, err := repo.UserExists(context.Background(), 123)
	require.NoError(t, err)
	require.False(t, exists, "DryRun 没有结果时不能误判用户存在")
	require.NotEmpty(t, log.queries)

	normalized := strings.ToLower(log.queries[len(log.queries)-1])
	require.Contains(t, normalized, `from "users"`)
	require.Contains(t, normalized, "deleted_at is null")
	require.Contains(t, normalized, "status")
	require.Contains(t, normalized, "active")
}

func TestAddFollowKeepsActiveUserGuard(t *testing.T) {
	log := &interactionSQLLogger{Interface: logger.Discard}
	repo := dryRunInteractionRepo(t, log)

	err := repo.AddFollow(context.Background(), 1, 2)
	require.ErrorIs(t, err, ErrUserNotFound)
	require.NotEmpty(t, log.queries)

	var insertSQL string
	for _, query := range log.queries {
		if strings.Contains(strings.ToLower(query), "insert into follows") {
			insertSQL = strings.ToLower(query)
			break
		}
	}
	require.NotEmpty(t, insertSQL)
	require.Contains(t, insertSQL, "deleted_at is null")
	require.Contains(t, insertSQL, "status = 'active'")
	require.Contains(t, insertSQL, "on conflict")
}

func TestFollowListsFilterInactiveUsers(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*GormRepository) error
	}{
		{
			name: "following",
			call: func(repo *GormRepository) error {
				_, err := repo.ListFollowing(context.Background(), 1, nil, 20)
				return err
			},
		},
		{
			name: "followers",
			call: func(repo *GormRepository) error {
				_, err := repo.ListFollowers(context.Background(), 1, nil, 20)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &interactionSQLLogger{Interface: logger.Discard}
			repo := dryRunInteractionRepo(t, log)
			require.ErrorIs(t, tc.call(repo), gorm.ErrDryRunModeUnsupported)
			require.NotEmpty(t, log.queries)

			normalized := strings.ToLower(log.queries[len(log.queries)-1])
			require.Contains(t, normalized, "join users")
			require.Contains(t, normalized, "deleted_at is null")
			require.Contains(t, normalized, "status")
			require.Contains(t, normalized, "active")
		})
	}
}

func TestIsFollowingManyFiltersInactiveFollowees(t *testing.T) {
	log := &interactionSQLLogger{Interface: logger.Discard}
	repo := dryRunInteractionRepo(t, log)

	following, err := repo.IsFollowingMany(context.Background(), 1, []int64{2, 3})
	require.ErrorIs(t, err, gorm.ErrDryRunModeUnsupported)
	require.Empty(t, following)
	require.NotEmpty(t, log.queries)

	normalized := strings.ToLower(log.queries[len(log.queries)-1])
	require.Contains(t, normalized, "join users")
	require.Contains(t, normalized, "followee_id in")
	require.Contains(t, normalized, "deleted_at is null")
	require.Contains(t, normalized, "status")
	require.Contains(t, normalized, "active")
}
