package observability

import (
	"github.com/lihualin/tokmall/internal/breaker"
	"github.com/prometheus/client_golang/prometheus"
)

// BreakerMetrics 暴露熔断器状态：
//
//	breaker_state{name}              0=closed 1=half_open 2=open
//	breaker_opens_total{name}        打开次数（下游故障次数）
//	breaker_rejections_total{name}   被快速拒绝的调用数（省下来的等待）
//	breaker_probes_total{name,result} half-open 探测结果
//
// rejections 是"熔断省了多少次必然失败的等待"的直接量化；
// 如果它一直是 0 而 opens 在涨，说明熔断阈值设置得过松。
type BreakerMetrics struct {
	state      *prometheus.GaugeVec
	opens      *prometheus.CounterVec
	rejections *prometheus.CounterVec
	probes     *prometheus.CounterVec
}

func NewBreakerMetrics(reg prometheus.Registerer) *BreakerMetrics {
	m := &BreakerMetrics{
		state: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "breaker_state", Help: "Circuit breaker state (0=closed, 1=half_open, 2=open)",
		}, []string{"name"}),
		opens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "breaker_opens_total", Help: "Times the circuit breaker opened",
		}, []string{"name"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "breaker_rejections_total", Help: "Calls rejected while the breaker was open",
		}, []string{"name"}),
		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "breaker_probes_total", Help: "Half-open probes by result",
		}, []string{"name", "result"}),
	}
	reg.MustRegister(m.state, m.opens, m.rejections, m.probes)
	return m
}

func (m *BreakerMetrics) State(name string, state breaker.State) {
	m.state.WithLabelValues(name).Set(stateValue(state))
}

func (m *BreakerMetrics) Opened(name string) { m.opens.WithLabelValues(name).Inc() }

func (m *BreakerMetrics) Rejected(name string) { m.rejections.WithLabelValues(name).Inc() }

func (m *BreakerMetrics) Probe(name string, success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	m.probes.WithLabelValues(name, result).Inc()
}

func stateValue(state breaker.State) float64 {
	switch state {
	case breaker.StateOpen:
		return 2
	case breaker.StateHalfOpen:
		return 1
	default:
		return 0
	}
}

// Collectors 暴露底层 collector：main 之外的调用方（测试、自定义注册器）
// 需要拿到真实指标名做校验，而不是靠字符串约定猜。
func (m *BreakerMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.state, m.opens, m.rejections, m.probes}
}
