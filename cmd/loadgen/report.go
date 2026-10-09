package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"
)

type latencyReport struct {
	Count     int     `json:"count"`
	P50MS     float64 `json:"p50_ms"`
	P95MS     float64 `json:"p95_ms"`
	P99MS     float64 `json:"p99_ms"`
	MaxMS     float64 `json:"max_ms"`
	SamplesUS []int64 `json:"samples_us"`
}
type stageReport struct {
	Success  int64         `json:"success"`
	Failures int64         `json:"failures"`
	Latency  latencyReport `json:"latency"`
}
type loadReport struct {
	Scenario           string                 `json:"scenario"`
	Concurrency        int                    `json:"concurrency"`
	SKUs               int                    `json:"sku_count"`
	StartedAt          time.Time              `json:"started_at"`
	EndedAt            time.Time              `json:"ended_at"`
	ElapsedSeconds     float64                `json:"elapsed_seconds"`
	ConfiguredSeconds  float64                `json:"configured_seconds"`
	Requests           int                    `json:"requests"`
	Success            int64                  `json:"success"`
	BusinessFailures   int64                  `json:"business_failures"`
	SystemFailures     int64                  `json:"system_failures"`
	SuccessRPS         float64                `json:"success_rps"`
	RequestRPS         float64                `json:"request_rps"`
	Transactions       int64                  `json:"successful_transactions"`
	OrdersPerSecond    float64                `json:"successful_orders_per_second"`
	RequestLatency     latencyReport          `json:"request_latency"`
	TransactionLatency latencyReport          `json:"successful_transaction_latency"`
	Stages             map[string]stageReport `json:"stages"`
	Statuses           map[string]int64       `json:"statuses"`
}

func latencySummary(samples []time.Duration) latencyReport {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	raw := make([]int64, len(samples))
	for i, d := range samples {
		raw[i] = d.Microseconds()
	}
	maxLatency := time.Duration(0)
	if len(sorted) > 0 {
		maxLatency = sorted[len(sorted)-1]
	}
	return latencyReport{Count: len(sorted), P50MS: float64(percentile(sorted, 0.50)) / float64(time.Millisecond), P95MS: float64(percentile(sorted, 0.95)) / float64(time.Millisecond), P99MS: float64(percentile(sorted, 0.99)) / float64(time.Millisecond), MaxMS: float64(maxLatency) / float64(time.Millisecond), SamplesUS: raw}
}

func (r *runner) snapshot() loadReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	elapsed := r.ended.Sub(r.started).Seconds()
	report := loadReport{Scenario: r.cfg.scenario, Concurrency: r.cfg.concurrency, SKUs: r.cfg.skus, StartedAt: r.started, EndedAt: r.ended, ElapsedSeconds: elapsed, ConfiguredSeconds: r.cfg.duration.Seconds(), Requests: len(r.latency), Success: r.success.Load(), BusinessFailures: r.business.Load(), SystemFailures: r.system.Load(), Transactions: r.transactions.Load(), RequestLatency: latencySummary(r.latency), TransactionLatency: latencySummary(r.transactionLatency), Statuses: map[string]int64{}, Stages: map[string]stageReport{}}
	if elapsed > 0 {
		report.RequestRPS = float64(report.Requests) / elapsed
		report.SuccessRPS = float64(report.Success) / elapsed
		report.OrdersPerSecond = float64(report.Transactions) / elapsed
	}
	for k, v := range r.statuses {
		report.Statuses[k] = v.Load()
	}
	for k, v := range r.stageLatency {
		report.Stages[k] = stageReport{Success: r.stageSuccess[k], Failures: r.stageFailures[k], Latency: latencySummary(v)}
	}
	return report
}

func (r *runner) writeReport(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(r.snapshot())
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// The coordinator snapshots metrics after setup and before releasing workers.
func (r *runner) awaitMeasurement(ctx context.Context) error {
	if r.cfg.readyFile != "" {
		if err := os.WriteFile(r.cfg.readyFile, []byte("ready"), 0600); err != nil {
			return err
		}
	}
	if r.cfg.startFile == "" {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(r.cfg.startFile)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
