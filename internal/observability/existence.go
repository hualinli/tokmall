package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ExistenceMetrics 暴露存在性观测与负缓存效果：
//
//	existence_lookup_total{namespace,result} —— 请求最终走到哪一层
//	  （bloom_miss / negative_cache_hit / database / warmup / ...）
//	existence_negative_cache_hit_total       —— 被负缓存挡掉的数量
//	existence_miss_marked_total              —— 回写负缓存的次数
//
// 这几个指标的用途不是"好看"，而是回答"防护到底有没有生效"：
// 重复请求同一不存在 ID 应命中负缓存；随机新 ID 仍会查数据库。
// bloom_miss 表示本地集合未命中后放行，不代表拒绝请求。
type ExistenceMetrics struct {
	lookup      *prometheus.CounterVec
	negative    *prometheus.CounterVec
	missMarked  *prometheus.CounterVec
	ready       *prometheus.GaugeVec
	filterCount *prometheus.GaugeVec
	filterBits  *prometheus.GaugeVec
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
		ready: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "existence_filter_ready", Help: "1 when the namespace filter finished its first full load",
		}, []string{"namespace"}),
		filterCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "existence_filter_items", Help: "Approximate number of items in the active bloom filter",
		}, []string{"namespace"}),
		filterBits: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "existence_filter_bits", Help: "Bit array size of the active bloom filter",
		}, []string{"namespace"}),
	}
	reg.MustRegister(m.lookup, m.negative, m.missMarked, m.ready, m.filterCount, m.filterBits)
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

// NewExistenceMetricsObserver 让 existence 的重建任务在每轮结束后刷新
// 过滤器状态指标，而不必让 existence 依赖 prometheus。
//
// 注意它复用同一个 ExistenceMetrics 实例：指标注册只能发生一次，
// 分别注册会在启动时 panic（duplicate metrics collector registration）。
func NewExistenceMetricsObserver(metrics *ExistenceMetrics) *existenceMetricsObserver {
	return &existenceMetricsObserver{metrics: metrics}
}

type existenceMetricsObserver struct {
	metrics *ExistenceMetrics
}

func (o *existenceMetricsObserver) Observe(namespace string, ready bool, items, bits uint64) {
	o.metrics.Observe(namespace, ready, items, bits)
}

// Observe 由周期任务调用，刷新过滤器自身的状态指标。
func (m *ExistenceMetrics) Observe(namespace string, ready bool, items, bits uint64) {
	if ready {
		m.ready.WithLabelValues(namespace).Set(1)
	} else {
		m.ready.WithLabelValues(namespace).Set(0)
	}
	m.filterCount.WithLabelValues(namespace).Set(float64(items))
	m.filterBits.WithLabelValues(namespace).Set(float64(bits))
}

// Collectors 暴露底层 collector：main 之外的调用方（测试、自定义注册器）
// 需要拿到真实指标名做校验，而不是靠字符串约定猜。
func (m *ExistenceMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.lookup, m.negative, m.missMarked, m.ready, m.filterCount, m.filterBits}
}
