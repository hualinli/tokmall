package order

import (
	"context"
	"log/slog"
	"time"
)

// RefundAutoApproveRepository 每次调用在自己的事务里处理一笔到期退款。
// 同意退款要改状态、回补库存、退券、写事件，不能像确认收货那样一条 UPDATE 批量做完。
type RefundAutoApproveRepository interface {
	ApproveOverdueRefund(ctx context.Context, now time.Time) (bool, error)
}

// RefundAutoApproveWorker 定期把超过 respond_by 仍未裁决的退款自动同意。
type RefundAutoApproveWorker struct {
	repo      RefundAutoApproveRepository
	interval  time.Duration
	batchSize int
	now       func() time.Time
}

func NewRefundAutoApproveWorker(repo RefundAutoApproveRepository, interval time.Duration, batchSize int) *RefundAutoApproveWorker {
	return &RefundAutoApproveWorker{repo: repo, interval: interval, batchSize: batchSize, now: time.Now}
}

func (w *RefundAutoApproveWorker) RunOnce(ctx context.Context) (int, error) {
	processed := 0
	for processed < w.batchSize {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		done, err := w.repo.ApproveOverdueRefund(ctx, w.now().UTC())
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

func (w *RefundAutoApproveWorker) Run(ctx context.Context) {
	if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "refund auto approve worker run failed", "err", err)
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
				slog.ErrorContext(ctx, "refund auto approve worker run failed", "err", err)
				continue
			}
			if processed > 0 {
				slog.InfoContext(ctx, "auto approved overdue refunds", "count", processed)
			}
		}
	}
}
