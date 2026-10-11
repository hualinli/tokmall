package observability

import (
	"database/sql"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func TestDBPoolRepeatedScrapesDoNotInflateCumulativeWaitCount(t *testing.T) {
	collector, ok := NewDBPoolCollector(nil, "test").(*dbPoolCollector)
	require.True(t, ok)
	read := func(waitCount int64) float64 {
		ch := make(chan prometheus.Metric, 8)
		collector.collectStats(sql.DBStats{WaitCount: waitCount}, ch)
		close(ch)
		for metric := range ch {
			var value dto.Metric
			require.NoError(t, metric.Write(&value))
			if value.Counter != nil {
				return value.GetCounter().GetValue()
			}
		}
		t.Fatal("wait counter missing")
		return 0
	}
	require.Equal(t, 7.0, read(7))
	require.Equal(t, 7.0, read(7))
	require.Equal(t, 9.0, read(9))
}

// 两个独立池必须能注册到同一出口，且保留各自的 db 标签。
func TestIndependentDBPoolsCanShareRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()
	for _, name := range []string{"tokmall", "tokmall_consumer"} {
		require.NoError(t, registry.Register(NewDBPoolCollector(new(sql.DB), name)))
	}
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 5)
	for _, family := range families {
		require.Len(t, family.Metric, 2)
		names := map[string]bool{}
		for _, metric := range family.Metric {
			require.Len(t, metric.Label, 1)
			require.Equal(t, "db", metric.Label[0].GetName())
			names[metric.Label[0].GetValue()] = true
		}
		require.Equal(t, map[string]bool{"tokmall": true, "tokmall_consumer": true}, names)
	}
}
