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
