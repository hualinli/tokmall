package video

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/testsupport"
)

func TestPostgresProbeQueueAndPublishGate(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	repo := NewRepository(db)
	ctx := context.Background()
	var authorID int64
	require.NoError(t, db.Raw("INSERT INTO users(email,username,password_hash,role) VALUES ('probe@test.local','probe','hash','USER') RETURNING id").Scan(&authorID).Error)
	insert := func(status Status) int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.Raw("INSERT INTO videos(author_id,title,object_key,status) VALUES (?,'probe','probe.mp4',?) RETURNING id", authorID, string(status)).Scan(&id).Error)
		return id
	}
	for range 4 {
		insert(StatusDeleted)
	}
	first, second := insert(StatusPendingReview), insert(StatusPendingReview)
	pending, err := repo.ListPendingProbe(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, first, pending[0].VideoID, "deleted objects must not block the queue")
	now := time.Now().UTC()
	err = repo.UpdateVideoStatus(ctx, first, authorID, StatusPendingReview, StatusPublished, &now)
	require.ErrorIs(t, err, ErrStatusConflict, "publish CAS requires completed validation")
	require.Error(t, db.Exec("UPDATE videos SET status = 'PUBLISHED', published_at = now() WHERE id = ?", first).Error, "database also rejects bypassing service validation")
	require.NoError(t, repo.DeferProbe(ctx, first, now.Add(time.Minute)))
	pending, err = repo.ListPendingProbe(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, second, pending[0].VideoID, "transient failure must let the next item progress")
	saved, err := repo.SaveProbeResult(ctx, second, ProbeOutcome{Status: "PASSED", DurationSeconds: 10, Width: 320, Height: 240, VideoCodec: "h264"})
	require.NoError(t, err)
	require.True(t, saved)
	require.NoError(t, repo.UpdateVideoStatus(ctx, second, authorID, StatusPendingReview, StatusPublished, &now))
	saved, err = repo.SaveProbeResult(ctx, first, ProbeOutcome{Status: "REJECTED", Error: "invalid media"})
	require.NoError(t, err)
	require.True(t, saved)
	err = repo.UpdateVideoStatus(ctx, first, authorID, StatusPendingReview, StatusPublished, &now)
	require.ErrorIs(t, err, ErrStatusConflict)
	// 从真实 SQL 读取校验结果，再验证作者视图的原因映射。
	rejected, err := repo.FindVideoByID(ctx, first)
	require.NoError(t, err)
	require.NotNil(t, rejected.MediaProbeError)
	require.Equal(t, "invalid media", *rejected.MediaProbeError)
	svc := NewService(repo, newFakeStorage(), time.Minute, "test-secret", nil)
	detail, err := svc.Get(ctx, authorID, first)
	require.NoError(t, err)
	require.Equal(t, ProbeRejected, detail.MediaValidation.Status)
	require.False(t, detail.MediaValidation.CanPublish)
	require.Equal(t, "视频未通过校验，请重新上传符合要求的视频。", *detail.MediaValidation.Reason)
	list, err := svc.ListMine(ctx, authorID, 20, "")
	require.NoError(t, err)
	var found bool
	for _, item := range list.Items {
		if item.VideoID == first {
			require.Equal(t, detail.MediaValidation, item.MediaValidation)
			found = true
		}
	}
	require.True(t, found)
}

func TestPostgresProbeMigrationQuarantinesLegacyPendingPublication(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	down, err := os.ReadFile("../../migrations/000022_probe_publish_gate.down.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(down)).Error)
	var authorID, videoID int64
	require.NoError(t, db.Raw("INSERT INTO users(email,username,password_hash,role) VALUES ('legacy@test.local','legacy','hash','USER') RETURNING id").Scan(&authorID).Error)
	require.NoError(t, db.Raw("INSERT INTO videos(author_id,title,object_key,status,published_at) VALUES (?,'legacy','legacy.mp4','PUBLISHED',now()) RETURNING id", authorID).Scan(&videoID).Error)
	up, err := os.ReadFile("../../migrations/000022_probe_publish_gate.up.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(up)).Error)
	video, err := NewRepository(db).FindVideoByID(context.Background(), videoID)
	require.NoError(t, err)
	require.Equal(t, StatusPendingReview, video.Status)
	require.Equal(t, ProbePending, video.MediaProbeStatus)
	require.Nil(t, video.PublishedAt)
}
