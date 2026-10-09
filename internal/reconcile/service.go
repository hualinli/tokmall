package reconcile

import (
	"context"
	"log/slog"
	"time"
)

// Runner 执行全部对账检查，并可选地重放"已发布但没有消费回执"的事件。
type Runner struct {
	repo Repository
	// grace 是"事件已发布但还没被消费"的容忍时间：正常投递 + 消费是秒级，
	// 超过它还没有回执，才值得怀疑事件真的丢了。
	grace time.Duration
	// consumer 是要核对回执的消费组名。它是**部署事实**而非库内细节：
	// consumed_events 主键是 (consumer, event_id)，不指明组名就无法判断
	// "有没有回执"。生产装配必须传应用实际使用的组名；空值退化为不限定。
	consumer string
	stats    Stats
	now      func() time.Time
}

// Stats 是对账指标接口（实现在 observability，避免本包依赖 prometheus）。
type Stats interface {
	CheckResult(name string, differences int)
	Run(ok bool, totalDifferences int)
	Replayed(count int)
}

type NopStats struct{}

func (NopStats) CheckResult(string, int) {}
func (NopStats) Run(bool, int)           {}
func (NopStats) Replayed(int)            {}

type Option func(*Runner)

func WithStats(stats Stats) Option {
	return func(r *Runner) { r.stats = stats }
}

func WithGrace(grace time.Duration) Option {
	return func(r *Runner) { r.grace = grace }
}

// WithConsumer 指定要核对回执的消费组名（通常来自 cfg.OutboxConsumerGroup）。
func WithConsumer(consumer string) Option {
	return func(r *Runner) { r.consumer = consumer }
}

func NewRunner(repo Repository, opts ...Option) *Runner {
	runner := &Runner{repo: repo, grace: time.Minute, stats: NopStats{}, now: time.Now}
	for _, opt := range opts {
		opt(runner)
	}
	return runner
}

// Run 执行全部检查并返回报告。任何一条检查出错都会记录并继续执行其它检查：
// 对账的价值在于"一次性看清所有问题"，而不是遇到第一个错误就停下。
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	started := r.now()
	report := &Report{GeneratedAt: started, Checks: []CheckResult{}}
	var firstErr error
	for _, definition := range checks(r.grace) {
		result, err := r.repo.RunCheck(ctx, definition.name, definition.description, definition.query, definition.args)
		if err != nil {
			slog.Error("reconcile check failed", "check", definition.name, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		report.Checks = append(report.Checks, result)
		r.stats.CheckResult(definition.name, len(result.Differences))
		if !result.OK() {
			slog.Error("reconcile drift detected", "check", definition.name,
				"differences", len(result.Differences), "sample", result.Differences[0].Columns)
		}
	}
	report.Duration = r.now().Sub(started)
	total := report.TotalDifferences()
	r.stats.Run(report.OK(), total)
	if report.OK() {
		slog.Info("reconcile clean", "checks", len(report.Checks), "duration", report.Duration)
	} else {
		slog.Error("reconcile found differences", "checks", len(report.Checks), "differences", total)
	}
	return report, firstErr
}

// ReplayMissingReceipts 是"对账驱动补偿"：把已发布但迟迟没有消费回执的事件
// 重新放回 Outbox。
//
// 为什么可以安全重放：消费回执与业务投影在同一事务里写入，因此"没有回执"
// 等价于"投影没有生效"；而重放沿用同一个 event_id，消费侧仍会去重。
// 这条路径覆盖的是 broker 侧真的丢数据（例如 Redis 未开 AOF 就重启）——
// 我们不用"相信 broker 不丢"来保证最终一致。
func (r *Runner) ReplayMissingReceipts(ctx context.Context) (int, error) {
	ids, err := r.repo.MissingReceipts(ctx, r.consumer, r.grace)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	replayed, err := r.repo.RequeueEvents(ctx, ids, r.now())
	if err != nil {
		return 0, err
	}
	r.stats.Replayed(replayed)
	slog.Warn("replayed events missing consumer receipts", "count", replayed, "consumer", r.consumer)
	return replayed, nil
}

// RebuildProjection 重建读模型。它只应在确认"事实侧可信、投影侧漂移"后使用：
// 重建会把投影里任何"事实侧没有"的内容抹掉。
func (r *Runner) RebuildProjection(ctx context.Context) (int64, error) {
	affected, err := r.repo.RebuildProjection(ctx)
	if err != nil {
		return 0, err
	}
	slog.Warn("author sales projection rebuilt from facts", "rows", affected)
	return affected, nil
}

// Worker 周期执行对账 + 补偿，让"数据是否一致"成为持续可观测的事实，
// 而不是上线前跑一次的手工动作。
type Worker struct {
	runner   *Runner
	interval time.Duration
	replay   bool
}

func NewWorker(runner *Runner, interval time.Duration, replay bool) *Worker {
	return &Worker{runner: runner, interval: interval, replay: replay}
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("reconcile worker stopped")
			return
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

// RunOnce 执行一轮对账（供 Worker 与 CLI 共用）。
func (w *Worker) RunOnce(ctx context.Context) *Report {
	report, err := w.runner.Run(ctx)
	if err != nil {
		slog.Error("reconcile run failed", "err", err)
	}
	if w.replay {
		if _, err := w.runner.ReplayMissingReceipts(ctx); err != nil {
			slog.Error("reconcile replay failed", "err", err)
		}
	}
	return report
}
