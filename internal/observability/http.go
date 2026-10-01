package observability

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTPMetrics 记录服务端请求耗时直方图。
//
// 为什么不直接用 hertz 自带 tracer 的指标：它的标签里带**原始路径**，
// /api/v1/videos/1 与 /api/v1/videos/2 是两个不同的时间序列——路由参数一多，
// 时间序列数量就爆炸（基数问题）。这里用路由模板（/api/v1/videos/:id）
// 作为标签，把基数钉死在"路由数量"这个量级上。
//
// 分桶覆盖 Web 语义的常见 SLO 阈值（10ms~10s），两侧都留出边界，
// 便于直接算 P95/P99 与"超过 500ms 的比例"。
type HTTPMetrics struct {
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	m := &HTTPMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_server_duration_seconds",
			Help:    "HTTP server request duration in seconds, labelled by route template",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route", "status"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_server_in_flight_requests",
			Help: "Number of HTTP requests currently being served",
		}),
	}
	reg.MustRegister(m.duration, m.inFlight)
	return m
}

// Observe 记录一次请求。status 用整数而不是字符串：调用方已经拿到 int，
// 在热路径上避免一次 strconv。
func (m *HTTPMetrics) Observe(method, route string, status int, seconds float64) {
	if route == "" {
		route = "unmatched"
	}
	m.duration.WithLabelValues(method, route, strconv.Itoa(status)).Observe(seconds)
}

// IncInFlight / DecInFlight 用于暴露"当前并发请求数"，
// 与 db_pool_in_use_connections 一起看能快速判断"是应用排队还是数据库排队"。
func (m *HTTPMetrics) IncInFlight() { m.inFlight.Inc() }
func (m *HTTPMetrics) DecInFlight() { m.inFlight.Dec() }

// Since 是一个小工具：把起始时间转成秒，避免每处都写 time.Since(x).Seconds()。
func Since(start time.Time) float64 { return time.Since(start).Seconds() }
