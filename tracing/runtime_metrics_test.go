//go:build unit

package tracing

import (
	"context"
	"strings"
	"testing"

	"github.com/LerianStudio/lib-observability/v3/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectMetricNames gathers every metric name emitted by a ManualReader-backed
// MeterProvider, used to assert the runtime instrumentation registered its
// go.* instruments.
func collectMetricNames(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var names []string

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}

	return names
}

func hasRuntimeMetric(names []string) bool {
	for _, n := range names {
		// contrib/runtime emits instruments under the "go." namespace
		// (e.g. go.memory.used, go.goroutine.count) plus process.runtime.*.
		if strings.HasPrefix(n, "go.") || strings.HasPrefix(n, "process.runtime.") {
			return true
		}
	}

	return false
}

// TestStartRuntimeMetrics_Disabled verifies the helper is a no-op when the
// EnableRuntimeMetrics toggle is off: no go.* instruments are registered.
func TestStartRuntimeMetrics_Disabled(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	cfg := TelemetryConfig{
		LibraryName:          "test-lib",
		EnableTelemetry:      true,
		EnableRuntimeMetrics: false,
		Logger:               log.NewNop(),
	}

	started := startRuntimeMetrics(cfg, mp)
	assert.False(t, started, "runtime metrics must not start when disabled")

	names := collectMetricNames(t, reader)
	assert.False(t, hasRuntimeMetric(names),
		"no go.*/process.runtime.* metrics expected when disabled: %v", names)
}

// TestStartRuntimeMetrics_Enabled verifies the helper registers the contrib
// runtime instruments when EnableRuntimeMetrics is on, so go.* metrics appear.
func TestStartRuntimeMetrics_Enabled(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	cfg := TelemetryConfig{
		LibraryName:          "test-lib",
		EnableTelemetry:      true,
		EnableRuntimeMetrics: true,
		Logger:               log.NewNop(),
	}

	started := startRuntimeMetrics(cfg, mp)
	require.True(t, started, "runtime metrics must start when enabled")

	names := collectMetricNames(t, reader)
	assert.True(t, hasRuntimeMetric(names),
		"expected go.*/process.runtime.* metrics after starting runtime instrumentation: %v", names)
}

// TestStartRuntimeMetrics_NilMeterProviderIsSafe verifies the helper degrades
// to a no-op (never panics) when the MeterProvider is nil.
func TestStartRuntimeMetrics_NilMeterProviderIsSafe(t *testing.T) {
	cfg := TelemetryConfig{
		LibraryName:          "test-lib",
		EnableTelemetry:      true,
		EnableRuntimeMetrics: true,
		Logger:               log.NewNop(),
	}

	assert.NotPanics(t, func() {
		started := startRuntimeMetrics(cfg, nil)
		assert.False(t, started, "nil MeterProvider must not start runtime metrics")
	})
}

// TestNewTelemetry_RuntimeMetricsConfigFieldDefaultsOff documents the zero-value
// Go convention: EnableRuntimeMetrics defaults to false unless explicitly set.
func TestNewTelemetry_RuntimeMetricsConfigFieldDefaultsOff(t *testing.T) {
	t.Parallel()

	cfg := TelemetryConfig{
		LibraryName:     "test-lib",
		EnableTelemetry: false,
		Logger:          log.NewNop(),
	}
	assert.False(t, cfg.EnableRuntimeMetrics,
		"EnableRuntimeMetrics must default to false (zero value)")
}
