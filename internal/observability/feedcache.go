package observability

import "github.com/prometheus/client_golang/prometheus"

// FeedCacheStats 把 feed 首页缓存命中率暴露为 prometheus counter，
// 满足 feed.CacheStats 接口（消费侧定义在 feed，实现在装配方）。
type FeedCacheStats struct {
	hit  prometheus.Counter
	miss prometheus.Counter
}

func NewFeedCacheStats(reg prometheus.Registerer) *FeedCacheStats {
	hit := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "feed_cache_hit_total",
		Help: "Total feed first-page cache hits",
	})
	miss := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "feed_cache_miss_total",
		Help: "Total feed first-page cache misses",
	})
	reg.MustRegister(hit, miss)
	return &FeedCacheStats{hit: hit, miss: miss}
}

func (s *FeedCacheStats) Hit()  { s.hit.Inc() }
func (s *FeedCacheStats) Miss() { s.miss.Inc() }

// Collectors 暴露底层 collector：main 之外的调用方（测试、自定义注册器）
// 需要拿到真实指标名做校验，而不是靠字符串约定猜。
func (m *FeedCacheStats) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.hit, m.miss}
}
