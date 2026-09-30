package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// OutboxRelayMetrics 实现 outbox.RelayStats：中继的投递、失败、DLQ 与积压。
// 这几个指标是"事件是否可靠分发"的唯一可观测证据：
//   - outbox_pending_events 持续上涨 = 投递链路断了（broker 挂/中继挂）；
//   - outbox_dead_events > 0     = 有事件需要人工介入，必须告警；
//   - outbox_oldest_pending_seconds = 最老待投递事件的年龄，比条数更能说明积压程度。
type OutboxRelayMetrics struct {
	published *prometheus.CounterVec
	failed    *prometheus.CounterVec
	dead      *prometheus.CounterVec
	pending   prometheus.Gauge
	deadGauge prometheus.Gauge
	oldest    prometheus.Gauge
}

func NewOutboxRelayMetrics(reg prometheus.Registerer) *OutboxRelayMetrics {
	m := &OutboxRelayMetrics{
		published: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_published_total", Help: "Total outbox events published to the transport",
		}, []string{"event_type"}),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_publish_failures_total", Help: "Total outbox publish attempts that failed",
		}, []string{"event_type"}),
		dead: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_dead_total", Help: "Total outbox events moved to the dead letter state",
		}, []string{"event_type"}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_pending_events", Help: "Outbox events waiting to be published",
		}),
		deadGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_dead_events", Help: "Outbox events in DEAD state awaiting manual replay",
		}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_oldest_pending_seconds", Help: "Age of the oldest pending outbox event",
		}),
	}
	reg.MustRegister(m.published, m.failed, m.dead, m.pending, m.deadGauge, m.oldest)
	return m
}

func (m *OutboxRelayMetrics) Published(eventType string, count int) {
	m.published.WithLabelValues(eventType).Add(float64(count))
}

func (m *OutboxRelayMetrics) Failed(eventType string) {
	m.failed.WithLabelValues(eventType).Inc()
}

func (m *OutboxRelayMetrics) Dead(eventType string) {
	m.dead.WithLabelValues(eventType).Inc()
}

func (m *OutboxRelayMetrics) Pending(pending, dead int64, oldestAge time.Duration) {
	m.pending.Set(float64(pending))
	m.deadGauge.Set(float64(dead))
	m.oldest.Set(oldestAge.Seconds())
}

// OutboxConsumerMetrics 实现 outbox.ConsumerStats：消费进度与 DLQ。
type OutboxConsumerMetrics struct {
	processed *prometheus.CounterVec
	duplicate *prometheus.CounterVec
	failed    *prometheus.CounterVec
	dlq       *prometheus.CounterVec
	lag       prometheus.Gauge
}

func NewOutboxConsumerMetrics(reg prometheus.Registerer) *OutboxConsumerMetrics {
	m := &OutboxConsumerMetrics{
		processed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_consumer_processed_total", Help: "Events successfully projected by the consumer",
		}, []string{"event_type"}),
		duplicate: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_consumer_duplicate_total", Help: "Events ignored because the receipt already existed",
		}, []string{"event_type"}),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_consumer_failures_total", Help: "Event handling failures (will be retried)",
		}, []string{"event_type"}),
		dlq: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_consumer_dead_letter_total", Help: "Events moved to the consumer DLQ",
		}, []string{"event_type"}),
		lag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_consumer_pending_messages", Help: "Messages delivered but not yet acknowledged",
		}),
	}
	reg.MustRegister(m.processed, m.duplicate, m.failed, m.dlq, m.lag)
	return m
}

func (m *OutboxConsumerMetrics) Processed(eventType string) {
	m.processed.WithLabelValues(eventType).Inc()
}

func (m *OutboxConsumerMetrics) Duplicated(eventType string) {
	m.duplicate.WithLabelValues(eventType).Inc()
}

func (m *OutboxConsumerMetrics) Failed(eventType string) {
	m.failed.WithLabelValues(eventType).Inc()
}

func (m *OutboxConsumerMetrics) DeadLettered(eventType string) {
	m.dlq.WithLabelValues(eventType).Inc()
}

func (m *OutboxConsumerMetrics) Lag(pending int64) {
	m.lag.Set(float64(pending))
}
