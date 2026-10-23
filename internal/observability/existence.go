package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ExistenceMetrics 暴露负缓存效果：
//
//	existence_lookup_total{namespace,result} —— 请求最终走到哪一层
//	  （negative_cache_hit / database / negative_cache_error）
//	existence_negative_cache_hit_total       —— 被负缓存挡掉的数量
//	existence_miss_marked_total              —— 回写负缓存的次数
//
// 重复请求同一不存在 ID 应命中负缓存；随机新 ID 仍会查数据库。
type ExistenceMetrics struct {
	lookup     *prometheus.CounterVec
	negative   *prometheus.CounterVec
	missMarked *prometheus.CounterVec
}

func NewExistenceMetrics(reg prometheus.Registerer) *ExistenceMetrics {
	m := &ExistenceMetrics{
		lookup: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "existence_lookup_total", Help: "Existence lookups by namespace and the layer that answered",
		}, []string{"namespace", "result"}),
		negative: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "existence_negative_cache_hit_total", Help: "Lookups rejected by the negative cache",
		}, []string{"namespace"}),
		missMarked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "existence_miss_marked_total", Help: "Confirmed misses written into the negative cache",
		}, []string{"namespace"}),
	}
	reg.MustRegister(m.lookup, m.negative, m.missMarked)
	return m
}

func (m *ExistenceMetrics) NegativeHit(namespace string) {
	m.negative.WithLabelValues(namespace).Inc()
}

func (m *ExistenceMetrics) MissMarked(namespace string) {
	m.missMarked.WithLabelValues(namespace).Inc()
}

func (m *ExistenceMetrics) Lookup(namespace, result string) {
	m.lookup.WithLabelValues(namespace, result).Inc()
}

// Collectors 暴露底层 collector：测试需要拿到真实指标名做校验。
func (m *ExistenceMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.lookup, m.negative, m.missMarked}
}
