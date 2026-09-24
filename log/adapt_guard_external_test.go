//go:build unit

package log_test

import (
	"context"
	"testing"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/metrics"
	obsruntime "github.com/LerianStudio/lib-observability/v4/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// panicsInFullLog is a consumer's own full Logger whose Log always panics.
type panicsInFullLog struct{ log.NopLogger }

func (*panicsInFullLog) Log(context.Context, int, string, ...any) { panic("boom") }

// panicsInLog is a Log-only consumer logger that always panics.
type panicsInLog struct{}

func (panicsInLog) Log(context.Context, int, string, ...any) { panic("boom") }

// recoveredPanics sums panic_recovered_total points carrying attrs.
func recoveredPanics(t *testing.T, reader *sdkmetric.ManualReader, attrs attribute.Set) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	var total int64

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != constant.MetricPanicRecoveredTotal {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)

			for _, point := range sum.DataPoints {
				if point.Attributes.Equals(&attrs) {
					total += point.Value
				}
			}
		}
	}

	return total
}

// TestAdapt_PanicInsideLoggerIncrementsRecoveredPanicCounter: a panic inside an
// adapted and inside a guarded logger each count on runtime's
// panic_recovered_total, labelled component=log and the method.
func TestAdapt_PanicInsideLoggerIncrementsRecoveredPanicCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	factory, err := metrics.NewMetricsFactory(provider.Meter("test"), nil)
	require.NoError(t, err)

	obsruntime.ResetPanicMetrics()
	obsruntime.InitPanicMetrics(factory)
	t.Cleanup(obsruntime.ResetPanicMetrics)

	require.NotPanics(t, func() {
		log.Adapt(panicsInLog{}).Log(context.Background(), log.LevelInfo, "m")
		log.Guard(&panicsInFullLog{}).Log(context.Background(), log.LevelInfo, "m")
	})

	assert.Equal(t, int64(2), recoveredPanics(t, reader, attribute.NewSet(
		attribute.String("component", "log"),
		attribute.String("goroutine_name", "Log"),
	)))
}
