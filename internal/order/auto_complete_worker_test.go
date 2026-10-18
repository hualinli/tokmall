package order

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeAutoCompleteRepository struct {
	mu      sync.Mutex
	results []int64
	errors  []error
	limits  []int
}

func (f *fakeAutoCompleteRepository) AutoCompleteDue(ctx context.Context, _ time.Time, limit int) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i := len(f.limits)
	f.limits = append(f.limits, limit)
	if i < len(f.errors) && f.errors[i] != nil {
		return 0, f.errors[i]
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	return 0, nil
}

func (f *fakeAutoCompleteRepository) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.limits)
}

func TestAutoCompleteWorkerRunOnceDrainsFullBatchesUntilShort(t *testing.T) {
	repo := &fakeAutoCompleteRepository{results: []int64{2, 2, 1, 2}}
	worker := NewAutoCompleteWorker(repo, time.Hour, 2)
	total, err := worker.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	require.Equal(t, []int{2, 2, 2}, repo.limits, "不满一批说明已无到期订单，不再继续查询")
}

func TestAutoCompleteWorkerRunOnceCapsBatchesPerTick(t *testing.T) {
	results := make([]int64, 20)
	for i := range results {
		results[i] = 1
	}
	repo := &fakeAutoCompleteRepository{results: results}
	worker := NewAutoCompleteWorker(repo, time.Hour, 1)
	total, err := worker.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(10), total)
	require.Equal(t, 10, repo.callCount(), "积压时一次 tick 最多处理 10 批，剩余留给下一次")
}

func TestAutoCompleteWorkerRunOnceReturnsErrorWithPartialCount(t *testing.T) {
	boom := errors.New("db down")
	repo := &fakeAutoCompleteRepository{results: []int64{3}, errors: []error{nil, boom}}
	worker := NewAutoCompleteWorker(repo, time.Hour, 3)
	total, err := worker.RunOnce(context.Background())
	require.ErrorIs(t, err, boom)
	require.Equal(t, int64(3), total)
}

func TestAutoCompleteWorkerRunRetriesAfterErrorAndStopsOnCancel(t *testing.T) {
	repo := &fakeAutoCompleteRepository{errors: []error{errors.New("temporary")}}
	worker := NewAutoCompleteWorker(repo, 5*time.Millisecond, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()

	require.Eventually(t, func() bool { return repo.callCount() >= 2 }, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}
