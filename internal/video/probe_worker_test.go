package video

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failingProbeRepo struct {
	retryAt time.Time
	saved   bool
}

func (r *failingProbeRepo) ListPendingProbe(context.Context, int) ([]ProbeCandidate, error) {
	return []ProbeCandidate{{VideoID: 1, ObjectKey: "missing"}}, nil
}
func (r *failingProbeRepo) SaveProbeResult(context.Context, int64, ProbeOutcome, ...func() error) (bool, error) {
	r.saved = true
	return true, nil
}
func (r *failingProbeRepo) DeferProbe(_ context.Context, _ int64, at time.Time) error {
	r.retryAt = at
	return nil
}

type failingProbeStorage struct{}

func (failingProbeStorage) DownloadToFile(context.Context, string, string) error {
	return errors.New("download failed")
}
func (failingProbeStorage) UploadFromFile(context.Context, string, string, int64) error { return nil }

func TestProbeDownloadFailureIsDeferredWithoutRejection(t *testing.T) {
	repo := &failingProbeRepo{}
	worker := NewProbeWorker(repo, failingProbeStorage{})
	before := time.Now()
	require.Equal(t, 1, worker.RunOnce(context.Background()))
	require.False(t, repo.saved)
	require.True(t, repo.retryAt.After(before.Add(59*time.Second)))
}

func TestPublishRequiresCompletedMediaValidation(t *testing.T) {
	for _, status := range []MediaProbeStatus{ProbePending, ProbeRejected, ProbePassed, ProbeSkipped} {
		t.Run(string(status), func(t *testing.T) {
			repo := newFakeRepo()
			repo.videos[1] = &Video{ID: 1, AuthorID: 42, Status: StatusPendingReview, MediaProbeStatus: status}
			svc := NewService(repo, newFakeStorage(), time.Minute, "secret", nil)
			_, err := svc.Publish(context.Background(), 42, 1)
			if status.AllowsPublish() {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrStatusConflict)
				require.Empty(t, repo.updateCalls)
			}
		})
	}
}

func TestCreateMediaProbeMode(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		repo, store := newFakeRepo(), newFakeStorage()
		svc := NewService(repo, store, time.Minute, "secret", nil)
		svc.SetMediaProbeEnabled(enabled)
		key := "videos/42/probe.mp4"
		registerTicket(t, repo, 42, key)
		store.objectSizes[key] = 1024
		created, err := svc.Create(context.Background(), 42, CreateParams{Title: "probe", ObjectKey: key, DurationSeconds: 10})
		require.NoError(t, err)
		if enabled {
			require.Equal(t, ProbePending, repo.videos[created.VideoID].MediaProbeStatus)
			_, err = svc.Publish(context.Background(), 42, created.VideoID)
			require.ErrorIs(t, err, ErrStatusConflict)
		} else {
			require.Equal(t, ProbeSkipped, repo.videos[created.VideoID].MediaProbeStatus)
			_, err = svc.Publish(context.Background(), 42, created.VideoID)
			require.NoError(t, err)
		}
	}
}
