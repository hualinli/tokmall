package video

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/testsupport"
)

func TestPostgresCleanupRecoversStorageFailure(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	var authorID, id int64
	require.NoError(t, db.Raw("INSERT INTO users(email,username,password_hash,role) VALUES ('cleanup@test.local','cleanup','hash','USER') RETURNING id").Scan(&authorID).Error)
	require.NoError(t, db.Raw("INSERT INTO videos(author_id,title,object_key,cover_key,status) VALUES (?,'cleanup','videos/cleanup.mp4','covers/cleanup.jpg','PENDING_REVIEW') RETURNING id", authorID).Scan(&id).Error)
	repo := NewRepository(db)
	store := newFakeStorage()
	store.deleteErr = errors.New("storage unavailable")
	svc := NewService(repo, store, time.Minute, "secret", nil)
	ctx := context.Background()
	require.NoError(t, svc.Delete(ctx, authorID, id))
	require.NoError(t, svc.Delete(ctx, authorID, id), "repeated deletion is idempotent")
	var count int64
	require.NoError(t, db.Model(&objectDeleteJob{}).Count(&count).Error)
	require.Equal(t, int64(2), count)
	worker := NewCleanupWorker(db, store)
	require.Error(t, worker.RunOnce(ctx))
	var failed objectDeleteJob
	require.NoError(t, db.Where("attempts=1").Take(&failed).Error)
	require.True(t, failed.AvailableAt.After(time.Now()))
	require.NotNil(t, failed.LastError)
	store.deleteErr = nil
	require.NoError(t, db.Model(&objectDeleteJob{}).Where("1=1").Update("available_at", time.Now().Add(-time.Minute)).Error)
	require.NoError(t, worker.RunOnce(ctx))
	require.NoError(t, db.Model(&objectDeleteJob{}).Count(&count).Error)
	require.Zero(t, count)
	require.ElementsMatch(t, []string{"videos/cleanup.mp4", "covers/cleanup.jpg"}, store.deletedKeys)
	called := false
	saved, err := repo.SaveProbeResult(ctx, id, ProbeOutcome{Status: "PASSED"}, func() error { called = true; return nil })
	require.NoError(t, err)
	require.False(t, saved)
	require.False(t, called, "a deleted video must never be rewritten by a late probe")
}

func TestPostgresTicketSealingSerializedAndRetryable(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	var authorID int64
	require.NoError(t, db.Raw("INSERT INTO users(email,username,password_hash,role) VALUES ('seal@test.local','sealtest','hash','USER') RETURNING id").Scan(&authorID).Error)
	repo := NewRepository(db)
	ctx := context.Background()
	key := "videos/seal.mp4"
	require.NoError(t, repo.CreateTicket(ctx, &UploadTicket{UserID: authorID, ObjectKey: key, ExpiresAt: time.Now().Add(time.Minute)}))
	require.Error(t, repo.ConsumeTicket(ctx, authorID, key, func() error { return errors.New("copy failed") }))
	var calls atomic.Int32
	var successes atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.ConsumeTicket(ctx, authorID, key, func() error { calls.Add(1); return nil })
			if err == nil {
				successes.Add(1)
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.ErrorIs(t, err, ErrTicketInvalid)
	}
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, int32(1), successes.Load())
	var job objectDeleteJob
	require.NoError(t, db.Where("object_key=?", "staging/"+key).Take(&job).Error)
	require.True(t, job.AvailableAt.After(time.Now()), "cleanup must wait until PUT expiry")
}
