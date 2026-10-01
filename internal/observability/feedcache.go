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

// HotFeedMetrics 暴露热榜的健康度：
//
//	hot_feed_score_updates_total{action}  计分写入（按互动类型）
//	hot_feed_window_rebuild_total{source} 窗口物化次数（scheduled=后台，on_demand=读路径回源）
//	hot_feed_window_entries               当前窗口条目数
//	hot_feed_source_total{source}         读路径数据来源（window=热榜窗口，timeline=降级）
//	hot_feed_degraded_total{reason}       降级次数与原因
//
// on_demand 重建次数是最值得盯的一条：它理想情况接近 0（窗口由后台每分钟物化）。
// 持续增长说明后台 worker 没在跑或窗口 TTL 太短，读路径在替它干活。
type HotFeedMetrics struct {
	scoreUpdates *prometheus.CounterVec
	rebuilds     *prometheus.CounterVec
	windowSize   prometheus.Gauge
	sources      *prometheus.CounterVec
	degraded     *prometheus.CounterVec
}

func NewHotFeedMetrics(reg prometheus.Registerer) *HotFeedMetrics {
	m := &HotFeedMetrics{
		scoreUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hot_feed_score_updates_total", Help: "Hot score increments by action type",
		}, []string{"action"}),
		rebuilds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hot_feed_window_rebuild_total", Help: "Hot window materializations by trigger",
		}, []string{"source"}),
		windowSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hot_feed_window_entries", Help: "Entries in the materialized hot window",
		}),
		sources: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hot_feed_source_total", Help: "Hot feed reads by data source",
		}, []string{"source"}),
		degraded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hot_feed_degraded_total", Help: "Hot feed degradations by reason",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.scoreUpdates, m.rebuilds, m.windowSize, m.sources, m.degraded)
	return m
}

func (m *HotFeedMetrics) HotScoreUpdate(action string) { m.scoreUpdates.WithLabelValues(action).Inc() }
func (m *HotFeedMetrics) HotWindowRebuild(source string) {
	m.rebuilds.WithLabelValues(source).Inc()
}
func (m *HotFeedMetrics) HotWindowSize(size int)      { m.windowSize.Set(float64(size)) }
func (m *HotFeedMetrics) HotFeedSource(source string) { m.sources.WithLabelValues(source).Inc() }
func (m *HotFeedMetrics) HotFeedDegraded(reason string) {
	m.degraded.WithLabelValues(reason).Inc()
}

func (m *HotFeedMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.scoreUpdates, m.rebuilds, m.windowSize, m.sources, m.degraded}
}

// MediaProbeMetrics 暴露媒体校验 worker 的结果：
//
//	media_probe_result_total{status}     校验结论（passed/rejected）
//	media_probe_skipped_total{reason}    跳过原因（tool_missing 等）
//	media_probe_duration_seconds         实测时长分布（发现超长视频）
//	media_faststart_applied_total        moov 前置处理次数
//
// rejected 增长说明有用户在传非视频/超长文件——这正是这条链路要挡的东西。
type MediaProbeMetrics struct {
	results   *prometheus.CounterVec
	skipped   *prometheus.CounterVec
	duration  prometheus.Histogram
	faststart prometheus.Counter
}

func NewMediaProbeMetrics(reg prometheus.Registerer) *MediaProbeMetrics {
	m := &MediaProbeMetrics{
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "media_probe_result_total", Help: "Media probe outcomes by status",
		}, []string{"status"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "media_probe_skipped_total", Help: "Media probes skipped by reason",
		}, []string{"reason"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "media_probe_duration_seconds",
			Help:    "Probed media duration in seconds",
			Buckets: []float64{1, 5, 15, 30, 60, 180, 600, 1800, 3600},
		}),
		faststart: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "media_faststart_applied_total", Help: "Objects rewritten with moov at the front",
		}),
	}
	reg.MustRegister(m.results, m.skipped, m.duration, m.faststart)
	return m
}

func (m *MediaProbeMetrics) ProbeResult(status string)  { m.results.WithLabelValues(status).Inc() }
func (m *MediaProbeMetrics) ProbeSkipped(reason string) { m.skipped.WithLabelValues(reason).Inc() }
func (m *MediaProbeMetrics) ProbeDuration(seconds float64) {
	if seconds > 0 {
		m.duration.Observe(seconds)
	}
}
func (m *MediaProbeMetrics) FaststartApplied() { m.faststart.Inc() }

func (m *MediaProbeMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.results, m.skipped, m.duration, m.faststart}
}
