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

// L1CacheMetrics 暴露进程内一级缓存的行为：
//
//	cache_l1_hit_total / miss_total      命中与未命中
//	cache_l1_eviction_total              LRU 淘汰（容量不够的信号）
//	cache_l1_stale_total                 因为版本号变化被判为陈旧
//	cache_l1_entries                     当前条目数
//
// stale 是最值得看的一条：它直接反映"别的实例写了新数据"的频率；
// 如果 stale 与 hit 同量级，说明写入太频繁、L1 基本没起到作用。
type L1CacheMetrics struct {
	hits      prometheus.Counter
	misses    prometheus.Counter
	evictions prometheus.Counter
	stales    prometheus.Counter
}

func NewL1CacheMetrics(reg prometheus.Registerer) *L1CacheMetrics {
	m := &L1CacheMetrics{
		hits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_l1_hit_total", Help: "In-process L1 cache hits",
		}),
		misses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_l1_miss_total", Help: "In-process L1 cache misses",
		}),
		evictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_l1_eviction_total", Help: "In-process L1 cache LRU evictions",
		}),
		stales: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_l1_stale_total", Help: "L1 entries invalidated because the data version changed",
		}),
	}
	reg.MustRegister(m.hits, m.misses, m.evictions, m.stales)
	return m
}

func (m *L1CacheMetrics) L1Hit()      { m.hits.Inc() }
func (m *L1CacheMetrics) L1Miss()     { m.misses.Inc() }
func (m *L1CacheMetrics) L1Eviction() { m.evictions.Inc() }
func (m *L1CacheMetrics) L1Stale()    { m.stales.Inc() }

func (m *L1CacheMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.hits, m.misses, m.evictions, m.stales}
}

// RedisMetrics 记录 Redis 侧的慢命令：按命令名计数。
// 它回答的问题是"缓存命中后瓶颈在哪条命令上"——命中率指标回答不了。
type RedisMetrics struct {
	slowCommands *prometheus.CounterVec
}

func NewRedisMetrics(reg prometheus.Registerer) *RedisMetrics {
	m := &RedisMetrics{
		slowCommands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "redis_slow_commands_total",
			Help: "Redis commands slower than the slow-command threshold, by command name",
		}, []string{"command"}),
	}
	reg.MustRegister(m.slowCommands)
	return m
}

func (m *RedisMetrics) RedisSlowCommand(command string) {
	m.slowCommands.WithLabelValues(command).Inc()
}

func (m *RedisMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.slowCommands}
}
