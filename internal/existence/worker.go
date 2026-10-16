package existence

import (
	"context"
	"log/slog"
	"time"
)

// LoaderFunc 把"当前存在的全部 ID"喂给 add。实现放在拥有该表的模块里
// （video.EachVideoID / product.EachProductID），本包不碰任何业务表。
// add 返回 error：调用方（仓储）用它把写过滤器的失败当普通错误处理。
type LoaderFunc func(ctx context.Context, add func(id int64) error) error

// Observer 让周期任务把过滤器自身状态同步到指标（实现在 observability）。
type Observer interface {
	Observe(namespace string, ready bool, items, bits uint64)
}

type NopObserver struct{}

func (NopObserver) Observe(string, bool, uint64, uint64) {}

// RebuildWorker 周期重建各命名空间的布隆过滤器。
//
// 为什么必须重建：布隆过滤器不支持删除，业务数据的删除/下架只会留下
// 越来越脏的位，误判率随时间上升。重建用一次全表遍历把误判率拉回设计值；
// 重建期间保留旧 active，成功后原子替换；本地 Bloom 不作为数据库不存在的证明。
//
// 启动时立即重建一次：在此之前 Ready()=false，Guard 会放行所有请求
// （宁可少挡一些，也不能把真实数据判成不存在）。
type RebuildWorker struct {
	guard    *Guard
	loaders  map[string]LoaderFunc
	interval time.Duration
	observer Observer
}

func NewRebuildWorker(guard *Guard, interval time.Duration, loaders map[string]LoaderFunc, observer Observer) *RebuildWorker {
	if observer == nil {
		observer = NopObserver{}
	}
	return &RebuildWorker{guard: guard, loaders: loaders, interval: interval, observer: observer}
}

func (w *RebuildWorker) Run(ctx context.Context) {
	w.rebuildAll(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("existence rebuild worker stopped")
			return
		case <-ticker.C:
			w.rebuildAll(ctx)
		}
	}
}

// RunOnce 供测试与运维手动触发。
func (w *RebuildWorker) RunOnce(ctx context.Context) error {
	var firstErr error
	for namespace, loader := range w.loaders {
		started := time.Now()

		err := w.guard.Rebuild(namespace, func(add func(id int64) error) error {
			return loader(ctx, add)
		})
		if err != nil {
			slog.Error("existence filter rebuild failed", "namespace", namespace, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		count, bits, hashes := w.guard.Snapshot(namespace)
		slog.Info("existence filter rebuilt", "namespace", namespace,
			"items", count, "bits", bits, "hashes", hashes, "duration", time.Since(started))
	}
	w.observe()
	return firstErr
}

func (w *RebuildWorker) rebuildAll(ctx context.Context) {
	if err := w.RunOnce(ctx); err != nil {
		slog.Warn("existence rebuild finished with errors", "err", err)
	}
}

func (w *RebuildWorker) observe() {
	for namespace := range w.loaders {
		count, bits, _ := w.guard.Snapshot(namespace)
		w.observer.Observe(namespace, w.guard.Ready(namespace), count, bits)
	}
}
