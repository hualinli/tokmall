package feed

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestPostgresFollowingTimeline verifies the actual SQL and service cursor boundary.
// The isolated schema is created only when integration testing is explicitly enabled.
func TestPostgresFollowingTimeline(t *testing.T) {
	if os.Getenv("TOKMALL_RUN_PG_INTEGRATION") != "1" {
		t.Skip("set TOKMALL_RUN_PG_INTEGRATION=1 and TOKMALL_TEST_DATABASE_DSN to run")
	}
	dsn := strings.TrimSpace(os.Getenv("TOKMALL_TEST_DATABASE_DSN"))
	require.NotEmpty(t, dsn)
	db := newFeedTestPostgres(t, dsn)
	ctx := context.Background()

	insertUser := func(name string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.Raw(`INSERT INTO users(email,username,password_hash,role)
			VALUES (?,?, 'hash','USER') RETURNING id`, name+"@example.test", name).Scan(&id).Error)
		return id
	}
	viewer := insertUser("viewer")
	authorA := insertUser("author-a")
	authorB := insertUser("author-b")
	outsider := insertUser("outsider")
	require.NoError(t, db.Exec("INSERT INTO follows(follower_id,followee_id) VALUES (?,?),(?,?)",
		viewer, authorA, viewer, authorB).Error)

	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	insertVideo := func(authorID int64, title, status string, publishedAt time.Time) int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.Raw(`INSERT INTO videos(author_id,title,object_key,status,published_at)
			VALUES (?,?,?,?,?) RETURNING id`, authorID, title, "videos/"+title+".mp4", status, publishedAt).Scan(&id).Error)
		return id
	}
	expected := make([]struct {
		id int64
		at time.Time
	}, 0, 8)
	for i := 0; i < 6; i++ {
		at := base.Add(-time.Duration(i) * time.Minute)
		expected = append(expected, struct {
			id int64
			at time.Time
		}{insertVideo(authorA, fmt.Sprintf("a%d", i+1), "PUBLISHED", at), at})
	}
	// Same timestamp across authors must use video ID as the stable tie-break.
	for i, at := range []time.Time{base.Add(-2 * time.Minute), base.Add(-5 * time.Minute)} {
		expected = append(expected, struct {
			id int64
			at time.Time
		}{insertVideo(authorB, fmt.Sprintf("b%d", i+1), "PUBLISHED", at), at})
	}
	insertVideo(outsider, "unfollowed", "PUBLISHED", base.Add(time.Minute))
	insertVideo(authorA, "pending", "PENDING_REVIEW", base.Add(2*time.Minute))
	insertVideo(authorA, "deleted", "DELETED", base.Add(3*time.Minute))
	sort.Slice(expected, func(i, j int) bool {
		if !expected[i].at.Equal(expected[j].at) {
			return expected[i].at.After(expected[j].at)
		}
		return expected[i].id > expected[j].id
	})

	svc := NewService(NewRepository(db), NewCodec("integration-cursor-secret"), fakeURLProvider{}, nil, nil, nil)
	var seen []int64
	cursor := ""
	for pageNum := 0; pageNum < 4; pageNum++ {
		page, err := svc.ListFollowing(ctx, viewer, 3, cursor)
		require.NoError(t, err)
		for _, item := range page.Items {
			seen = append(seen, item.VideoID)
		}
		if !page.HasMore {
			require.Nil(t, page.NextCursor)
			break
		}
		cursor = cursorOrEmpty(page.NextCursor)
		require.NotEmpty(t, cursor)
		if pageNum == 0 {
			// A new item above the cursor must not appear on older pages.
			insertVideo(authorA, "new-after-page-one", "PUBLISHED", base.Add(4*time.Minute))
		}
	}
	want := make([]int64, len(expected))
	for i, row := range expected {
		want[i] = row.id
	}
	require.Equal(t, want, seen, "all followed videos, including the sixth from one author, appear exactly once")

	empty, err := svc.ListFollowing(ctx, outsider, 3, "")
	require.NoError(t, err)
	require.Empty(t, empty.Items)
	require.False(t, empty.HasMore)
	quietViewer := insertUser("quiet-viewer")
	quietAuthor := insertUser("quiet-author")
	require.NoError(t, db.Exec("INSERT INTO follows(follower_id,followee_id) VALUES (?,?)", quietViewer, quietAuthor).Error)
	insertVideo(quietAuthor, "quiet-pending", "PENDING_REVIEW", base)
	empty, err = svc.ListFollowing(ctx, quietViewer, 3, "")
	require.NoError(t, err)
	require.Empty(t, empty.Items, "followed authors without published work yield an empty page")
	require.False(t, empty.HasMore)

	// More than the merge threshold exercises the global time-index path.
	wideViewer := insertUser("wide-viewer")
	wideWant := make([]int64, 0, followingMergeMaxAuthors+1)
	for i := 0; i < followingMergeMaxAuthors+1; i++ {
		author := insertUser(fmt.Sprintf("wide-author-%d", i))
		require.NoError(t, db.Exec("INSERT INTO follows(follower_id,followee_id) VALUES (?,?)", wideViewer, author).Error)
		wideWant = append(wideWant, insertVideo(author, fmt.Sprintf("wide-video-%d", i), "PUBLISHED", base.Add(time.Duration(i)*time.Minute)))
	}
	for i, j := 0, len(wideWant)-1; i < j; i, j = i+1, j-1 {
		wideWant[i], wideWant[j] = wideWant[j], wideWant[i]
	}
	var wideSeen []int64
	cursor = ""
	for pageNum := 0; pageNum < 8; pageNum++ {
		page, err := svc.ListFollowing(ctx, wideViewer, 5, cursor)
		require.NoError(t, err)
		for _, item := range page.Items {
			wideSeen = append(wideSeen, item.VideoID)
		}
		if !page.HasMore {
			break
		}
		cursor = cursorOrEmpty(page.NextCursor)
	}
	require.Equal(t, wideWant, wideSeen, "global-index path must preserve the same complete timeline")
}

func newFeedTestPostgres(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := "tokmall_feed_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)
	u, err := url.Parse(dsn)
	if err == nil && u.Scheme != "" {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	sort.Strings(paths)
	for _, path := range paths {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoErrorf(t, db.Exec(string(body)).Error, "apply %s", filepath.Base(path))
	}
	return db
}
