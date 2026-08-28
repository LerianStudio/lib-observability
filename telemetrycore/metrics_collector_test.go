//go:build unit

package telemetrycore

import (
	"sync"
	"testing"
	"time"

	"github.com/LerianStudio/lib-observability/v4/metrics"
	"github.com/LerianStudio/lib-observability/v4/tracing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// TestGetMetricsCollectionInterval tests the getMetricsCollectionInterval function.
func TestGetMetricsCollectionInterval(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected time.Duration
	}{
		{
			name:     "default when not set",
			envValue: "",
			expected: DefaultMetricsCollectionInterval,
		},
		{
			name:     "valid duration in seconds",
			envValue: "10s",
			expected: 10 * time.Second,
		},
		{
			name:     "valid duration in milliseconds",
			envValue: "500ms",
			expected: 500 * time.Millisecond,
		},
		{
			name:     "valid duration in minutes",
			envValue: "1m",
			expected: 1 * time.Minute,
		},
		{
			name:     "invalid format falls back to default",
			envValue: "invalid",
			expected: DefaultMetricsCollectionInterval,
		},
		{
			name:     "zero value falls back to default",
			envValue: "0s",
			expected: DefaultMetricsCollectionInterval,
		},
		{
			name:     "negative value falls back to default",
			envValue: "-5s",
			expected: DefaultMetricsCollectionInterval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv("METRICS_COLLECTION_INTERVAL", tt.envValue)
			} else {
				t.Setenv("METRICS_COLLECTION_INTERVAL", "")
			}

			result := getMetricsCollectionInterval()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func resetMetricsCollectorState() {
	metricsCollectorMu.Lock()
	defer metricsCollectorMu.Unlock()

	if metricsCollectorStarted && metricsCollectorShutdown != nil {
		close(metricsCollectorShutdown)
		time.Sleep(50 * time.Millisecond)
	}

	metricsCollectorShutdown = nil
	metricsCollectorStarted = false
	metricsCollectorOnce = &sync.Once{}
	metricsCollectorInitErr = nil
}

func TestEnsureMetricsCollector_ReturnsErrorWhenMetricsFactoryNil(t *testing.T) {
	resetMetricsCollectorState()
	t.Cleanup(resetMetricsCollectorState)

	tl := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library", EnableTelemetry: true},
		MeterProvider:   sdkmetric.NewMeterProvider(),
	}

	err := EnsureMetricsCollector(tl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MetricsFactory is nil")
	assert.False(t, metricsCollectorStarted)
}

func TestEnsureMetricsCollector_NoMeterProviderReturnsNil(t *testing.T) {
	resetMetricsCollectorState()
	t.Cleanup(resetMetricsCollectorState)

	tl := &tracing.Telemetry{}
	require.NoError(t, EnsureMetricsCollector(tl))
	assert.False(t, metricsCollectorStarted)
}

func TestStopMetricsCollector_AllowsRestart(t *testing.T) {
	resetMetricsCollectorState()
	t.Cleanup(resetMetricsCollectorState)

	tl := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library", EnableTelemetry: true},
		MeterProvider:   sdkmetric.NewMeterProvider(),
		MetricsFactory:  metrics.NewNopFactory(),
	}

	require.NoError(t, EnsureMetricsCollector(tl))
	assert.True(t, metricsCollectorStarted)

	StopMetricsCollector()
	assert.False(t, metricsCollectorStarted)

	require.NoError(t, EnsureMetricsCollector(tl))
	assert.True(t, metricsCollectorStarted)
}
