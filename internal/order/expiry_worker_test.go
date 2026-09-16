package order

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeExpiryRepository struct {
	mu      sync.Mutex
	results []bool
	errors  []error
	calls   int
	called  chan struct{}
}

func (f *fakeExpiryRepository) ExpireNext(ctx context.Context, _ time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	if i < len(f.errors) && f.errors[i] != nil {
		return false, f.errors[i]
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	return false, nil
}

func (f *fakeExpiryRepository) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestExpiryWorkerRunOnceStopsAtEmptyOrBatchBoundary(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		repo := &fakeExpiryRepository{results: []bool{true, true, false}}
		worker := NewExpiryWorker(repo, time.Hour, 10)
		count, err := worker.RunOnce(context.Background())
		require.NoError(t, err)
		require.Equal(t, 2, count)
		require.Equal(t, 3, repo.callCount())
	})
	t.Run("batch", func(t *testing.T) {
		repo := &fakeExpiryRepository{results: []bool{true, true, true}}
		worker := NewExpiryWorker(repo, time.Hour, 2)
		count, err := worker.RunOnce(context.Background())
		require.NoError(t, err)
		require.Equal(t, 2, count)
		require.Equal(t, 2, repo.callCount())
	})
}

func TestExpiryWorkerRunStopsPromptlyOnCancellation(t *testing.T) {
	repo := &fakeExpiryRepository{}
	worker := NewExpiryWorker(repo, time.Hour, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestExpiryWorkerRunRetriesDatabaseErrorOnNextTick(t *testing.T) {
	repo := &fakeExpiryRepository{errors: []error{errors.New("temporary")}, called: make(chan struct{}, 4)}
	worker := NewExpiryWorker(repo, 5*time.Millisecond, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()

	require.Eventually(t, func() bool { return repo.callCount() >= 2 }, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestExpiryWorkerRunOnceHonorsCanceledContext(t *testing.T) {
	repo := &fakeExpiryRepository{}
	worker := NewExpiryWorker(repo, time.Hour, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	count, err := worker.RunOnce(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, count)
	require.Zero(t, repo.callCount())
}
