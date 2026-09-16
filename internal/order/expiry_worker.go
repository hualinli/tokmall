package order

import (
	"context"
	"log/slog"
	"time"
)

type ExpiryRepository interface {
	ExpireNext(ctx context.Context, now time.Time) (bool, error)
}

type ExpiryWorker struct {
	repo      ExpiryRepository
	interval  time.Duration
	batchSize int
	now       func() time.Time
}

func NewExpiryWorker(repo ExpiryRepository, interval time.Duration, batchSize int) *ExpiryWorker {
	return &ExpiryWorker{repo: repo, interval: interval, batchSize: batchSize, now: time.Now}
}

func (w *ExpiryWorker) RunOnce(ctx context.Context) (int, error) {
	processed := 0
	for processed < w.batchSize {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		done, err := w.repo.ExpireNext(ctx, w.now().UTC())
		if err != nil {
			return processed, err
		}
		if !done {
			return processed, nil
		}
		processed++
	}
	return processed, nil
}

func (w *ExpiryWorker) Run(ctx context.Context) {
	if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
		slog.Error("order expiry worker run failed", "err", err)
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processed, err := w.RunOnce(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Error("order expiry worker run failed", "err", err)
				continue
			}
			if processed > 0 {
				slog.Info("expired pending orders", "count", processed)
			}
		}
	}
}
