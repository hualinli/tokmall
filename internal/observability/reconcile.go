package observability

import "github.com/prometheus/client_golang/prometheus"

// ReconcileMetrics 把对账结果变成可告警的指标：
//   - reconcile_differences{check} 任何一项 > 0 都意味着"账对不上"；
//   - reconcile_last_run_ok 让"对账任务本身是否在跑"可被观测
//     （任务挂了和账是对的，是两种完全不同的状态，不能都表现为"没有告警"）。
type ReconcileMetrics struct {
	differences *prometheus.GaugeVec
	lastOK      prometheus.Gauge
	lastRun     prometheus.Gauge
	runs        *prometheus.CounterVec
	replayed    prometheus.Counter
}

func NewReconcileMetrics(reg prometheus.Registerer) *ReconcileMetrics {
	m := &ReconcileMetrics{
		differences: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "reconcile_differences", Help: "Differences found by each reconciliation check",
		}, []string{"check"}),
		lastOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reconcile_last_run_ok", Help: "1 when the last reconciliation run found no differences",
		}),
		lastRun: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reconcile_last_run_timestamp_seconds", Help: "Unix time of the last reconciliation run",
		}),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reconcile_runs_total", Help: "Reconciliation runs by result",
		}, []string{"result"}),
		replayed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reconcile_replayed_events_total", Help: "Events replayed because they had no consumer receipt",
		}),
	}
	reg.MustRegister(m.differences, m.lastOK, m.lastRun, m.runs, m.replayed)
	return m
}

func (m *ReconcileMetrics) CheckResult(name string, differences int) {
	m.differences.WithLabelValues(name).Set(float64(differences))
}

func (m *ReconcileMetrics) Run(ok bool, _ int) {
	result := "drift"
	if ok {
		result = "ok"
		m.lastOK.Set(1)
	} else {
		m.lastOK.Set(0)
	}
	m.runs.WithLabelValues(result).Inc()
	m.lastRun.SetToCurrentTime()
}

func (m *ReconcileMetrics) Replayed(count int) {
	m.replayed.Add(float64(count))
}

// Collectors 暴露底层 collector：main 之外的调用方（测试、自定义注册器）
// 需要拿到真实指标名做校验，而不是靠字符串约定猜。
func (m *ReconcileMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.differences, m.lastOK, m.lastRun, m.runs, m.replayed}
}
