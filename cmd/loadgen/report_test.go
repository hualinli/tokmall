package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReportUsesActualElapsedAndSuccessfulOrders(t *testing.T) {
	start := time.Now()
	r := &runner{cfg: config{scenario: "commerce", duration: time.Second, concurrency: 2, skus: 2}, started: start, ended: start.Add(2 * time.Second), statuses: map[string]*atomic.Int64{}}
	r.record("order_created", 10*time.Millisecond, kindSuccess)
	r.record("order_paid", 30*time.Millisecond, kindSuccess)
	r.record("order_conflict", time.Millisecond, kindBusiness)
	r.transactions.Store(1)
	r.transactionLatency = []time.Duration{40 * time.Millisecond}
	report := r.snapshot()
	require.Equal(t, 1.0, report.SuccessRPS)
	require.Equal(t, 0.5, report.OrdersPerSecond)
	require.Equal(t, 1.5, report.RequestRPS)
	require.Equal(t, int64(1), report.Stages["pay"].Success)
	require.Equal(t, int64(1), report.Stages["create"].Failures)
	require.Equal(t, 40.0, report.TransactionLatency.P99MS)
}

func TestFailedPaymentIsNotASuccessfulTransaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer server.Close()
	r := &runner{cfg: config{base: server.URL}, client: server.Client(), statuses: map[string]*atomic.Int64{}}
	require.False(t, r.payOrder(context.Background(), "token", 1, 100, "key"))
	require.Equal(t, int64(1), r.system.Load())
	require.Zero(t, r.transactions.Load())
}
