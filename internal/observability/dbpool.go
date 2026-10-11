package observability

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// NewDBPoolCollector 暴露 database/sql 连接池指标，
// 注册到默认 registry，与 hertz 的 HTTP 指标共用一个 /metrics 出口。
func NewDBPoolCollector(db *sql.DB, dbName string) prometheus.Collector {
	return &dbPoolCollector{
		dbName: dbName,
		db:     db,
		openConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "db",
			Subsystem:   "pool",
			Name:        "open_connections",
			ConstLabels: prometheus.Labels{"db": dbName},
			Help:        "number of open connections (in use + idle)",
		}, nil),
		inUse: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "db",
			Subsystem:   "pool",
			Name:        "in_use_connections",
			ConstLabels: prometheus.Labels{"db": dbName},
			Help:        "number of connections currently in use",
		}, nil),
		idle: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "db",
			Subsystem:   "pool",
			Name:        "idle_connections",
			ConstLabels: prometheus.Labels{"db": dbName},
			Help:        "number of idle connections",
		}, nil),
		waitCount: prometheus.NewDesc("db_pool_wait_count_total",
			"total number of times a caller waited for a connection", nil, prometheus.Labels{"db": dbName}),
		waitDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "db",
			Subsystem:   "pool",
			Name:        "wait_duration_seconds",
			ConstLabels: prometheus.Labels{"db": dbName},
			Help:        "total time callers waited for a connection (sum)",
		}, nil),
	}
}

type dbPoolCollector struct {
	dbName          string
	db              *sql.DB
	openConnections *prometheus.GaugeVec
	inUse           *prometheus.GaugeVec
	idle            *prometheus.GaugeVec
	waitCount       *prometheus.Desc
	waitDuration    *prometheus.GaugeVec
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	c.openConnections.Describe(ch)
	c.inUse.Describe(ch)
	c.idle.Describe(ch)
	ch <- c.waitCount
	c.waitDuration.Describe(ch)
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	c.collectStats(c.db.Stats(), ch)
}

func (c *dbPoolCollector) collectStats(stats sql.DBStats, ch chan<- prometheus.Metric) {
	c.openConnections.WithLabelValues().Set(float64(stats.OpenConnections))
	c.inUse.WithLabelValues().Set(float64(stats.InUse))
	c.idle.WithLabelValues().Set(float64(stats.Idle))
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(stats.WaitCount))
	c.waitDuration.WithLabelValues().Set(stats.WaitDuration.Seconds())
	c.openConnections.Collect(ch)
	c.inUse.Collect(ch)
	c.idle.Collect(ch)
	c.waitDuration.Collect(ch)
}
