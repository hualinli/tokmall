package order

import (
	"context"
	"log/slog"
	"time"
)

type AutoCompleteRepository interface {
	AutoCompleteDue(ctx context.Context, now time.Time, limit int) (int64, error)
}

// AutoCompleteWorker 定期把超过自动确认截止时间的 FULFILLING 订单推进到 COMPLETED。
//
// 这里只用轮询、不用 Outbox 延迟事件：截止时间会被"拒绝退款"顺延，
// 已发出的延迟事件无法撤回；而且确认收货只需要分钟级精度，按部分索引扫描已经足够。
type AutoCompleteWorker struct {
	repo      AutoCompleteRepository
	interval  time.Duration
	batchSize int
	now       func() time.Time
}

func NewAutoCompleteWorker(repo AutoCompleteRepository, interval time.Duration, batchSize int) *AutoCompleteWorker {
	return &AutoCompleteWorker{repo: repo, interval: interval, batchSize: batchSize, now: time.Now}
}

// RunOnce 持续处理到期订单，直到某一批不满或累计达到 batchSize 的 10 倍，
// 避免积压时一次 tick 无限占用连接。
func (w *AutoCompleteWorker) RunOnce(ctx context.Context) (int64, error) {
	var total int64
	for range 10 {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		completed, err := w.repo.AutoCompleteDue(ctx, w.now().UTC(), w.batchSize)
		if err != nil {
			return total, err
		}
		total += completed
		if completed < int64(w.batchSize) {
			return total, nil
		}
	}
	return total, nil
}

func (w *AutoCompleteWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		completed, err := w.RunOnce(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			slog.ErrorContext(ctx, "order auto complete worker run failed", "err", err)
		case completed > 0:
			slog.InfoContext(ctx, "auto completed fulfilled orders", "count", completed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
