//go:build unit

package metrics

import (
	"context"
	"math"
	"testing"

	"github.com/LerianStudio/lib-observability/v3/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newCollectingFactory returns a factory backed by the OTEL SDK with a manual
// reader, so recorded values and attributes can be asserted on for real.
func newCollectingFactory(t *testing.T) (*MetricsFactory, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	factory, err := NewMetricsFactory(provider.Meter("universal-test"), log.NewNop())
	require.NoError(t, err)

	return factory, reader
}

func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()

	var rm metricdata.ResourceMetrics

	require.NoError(t, reader.Collect(context.Background(), &rm))

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m
			}
		}
	}

	t.Fatalf("metric %q was not collected", name)

	return metricdata.Metrics{}
}

func TestContractMetricsAddCounter(t *testing.T) {
	factory, reader := newCollectingFactory(t)
	recorder := AsRecorder(factory)

	ctx := context.Background()

	require.NoError(t, recorder.AddCounter(ctx, "requests_total", "total requests", "1", map[string]string{"route": "/health"}, 3))
	require.NoError(t, recorder.AddCounter(ctx, "requests_total", "total requests", "1", map[string]string{"route": "/health"}, 4))

	collected := collectMetric(t, reader, "requests_total")
	assert.Equal(t, "total requests", collected.Description)
	assert.Equal(t, "1", collected.Unit)

	sum, ok := collected.Data.(metricdata.Sum[int64])
	require.True(t, ok, "counter must collect as an int64 sum")
	require.Len(t, sum.DataPoints, 1, "same name + same labels must reuse one instrument and one series")
	assert.Equal(t, int64(7), sum.DataPoints[0].Value)

	route, found := sum.DataPoints[0].Attributes.Value("route")
	require.True(t, found)
	assert.Equal(t, "/health", route.AsString())
}

func TestContractMetricsAddCounterRejectsNegativeDelta(t *testing.T) {
	factory, _ := newCollectingFactory(t)

	err := AsRecorder(factory).AddCounter(context.Background(), "requests_total", "", "1", nil, -1)
	require.ErrorIs(t, err, ErrNegativeCounterValue)
	assert.ErrorContains(t, err, "requests_total")
}

func TestContractMetricsSetGauge(t *testing.T) {
	factory, reader := newCollectingFactory(t)
	recorder := AsRecorder(factory)

	ctx := context.Background()

	require.NoError(t, recorder.SetGauge(ctx, "queue_depth", "pending messages", "1", nil, 10))
	require.NoError(t, recorder.SetGauge(ctx, "queue_depth", "pending messages", "1", nil, 4))

	collected := collectMetric(t, reader, "queue_depth")

	gauge, ok := collected.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "gauge must collect as an int64 gauge")
	require.Len(t, gauge.DataPoints, 1)
	assert.Equal(t, int64(4), gauge.DataPoints[0].Value, "gauge holds the last value set")
}

func TestContractMetricsRecordHistogram(t *testing.T) {
	factory, reader := newCollectingFactory(t)

	buckets := []float64{10, 50, 100}
	err := AsRecorder(factory).RecordHistogram(
		context.Background(), "request_duration_ms", "request duration", "ms",
		map[string]string{"route": "/health"}, 42, buckets,
	)
	require.NoError(t, err)

	collected := collectMetric(t, reader, "request_duration_ms")
	assert.Equal(t, "ms", collected.Unit)

	histogram, ok := collected.Data.(metricdata.Histogram[int64])
	require.True(t, ok, "histogram must collect as an int64 histogram")
	require.Len(t, histogram.DataPoints, 1)
	assert.Equal(t, uint64(1), histogram.DataPoints[0].Count)
	assert.Equal(t, int64(42), histogram.DataPoints[0].Sum)
	assert.Equal(t, buckets, histogram.DataPoints[0].Bounds)
}

func TestContractMetricsRecordHistogramNilBucketsUsesFactoryDefaults(t *testing.T) {
	factory, reader := newCollectingFactory(t)

	require.NoError(t, AsRecorder(factory).RecordHistogram(
		context.Background(), "transaction_latency", "", "ms", nil, 5, nil,
	))

	collected := collectMetric(t, reader, "transaction_latency")

	histogram, ok := collected.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	require.Len(t, histogram.DataPoints, 1)
	assert.Equal(t, DefaultLatencyBuckets, histogram.DataPoints[0].Bounds)
}

// TestContractMetricsRecordHistogramRounding pins the DIVERGENCE between the
// float64 parameter and the int64 instrument underneath: values are rounded
// half away from zero, so sub-integer measurements (durations in seconds)
// collapse toward 0. See the precision note on Recorder.
func TestContractMetricsRecordHistogramRounding(t *testing.T) {
	tests := []struct {
		name     string
		value    float64
		expected int64
	}{
		{name: "integral", value: 42, expected: 42},
		{name: "rounds half up", value: 2.5, expected: 3},
		{name: "rounds half away from zero when negative", value: -2.5, expected: -3},
		{name: "rounds down", value: 2.4, expected: 2},
		{name: "sub-second duration collapses to zero", value: 0.004, expected: 0},
		{name: "negative sub-integer collapses to zero", value: -0.004, expected: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := int64FromFloat(tt.value)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestContractMetricsRecordHistogramRejectsUnrepresentableValues(t *testing.T) {
	factory, _ := newCollectingFactory(t)
	recorder := AsRecorder(factory)

	values := map[string]float64{
		"NaN":               math.NaN(),
		"positive infinity": math.Inf(1),
		"negative infinity": math.Inf(-1),
		"above int64":       math.MaxFloat64,
		"below int64":       -math.MaxFloat64,
		"exactly 2^63":      float64(math.MaxInt64),
	}

	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			_, err := int64FromFloat(value)
			require.ErrorIs(t, err, ErrHistogramValueNotRepresentable)

			err = recorder.RecordHistogram(context.Background(), "bad_histogram", "", "1", nil, value, nil)
			require.ErrorIs(t, err, ErrHistogramValueNotRepresentable)
			assert.ErrorContains(t, err, "bad_histogram")
		})
	}
}

func TestContractMetricsRecordHistogramAcceptsInt64Bounds(t *testing.T) {
	min64, err := int64FromFloat(float64(math.MinInt64))
	require.NoError(t, err)
	assert.Equal(t, int64(math.MinInt64), min64)
}

func TestContractMetricsNilFactoryIsNoOp(t *testing.T) {
	recorder := AsRecorder(nil)
	require.NotNil(t, recorder)

	ctx := context.Background()

	assert.NotPanics(t, func() {
		require.NoError(t, recorder.AddCounter(ctx, "c", "", "1", map[string]string{"a": "b"}, 1))
		require.NoError(t, recorder.SetGauge(ctx, "g", "", "1", nil, 1))
		require.NoError(t, recorder.RecordHistogram(ctx, "h", "", "1", nil, math.NaN(), nil))
	})
}

func TestContractMetricsNilAttributesAreAccepted(t *testing.T) {
	factory, reader := newCollectingFactory(t)

	require.NoError(t, AsRecorder(factory).AddCounter(context.Background(), "no_labels_total", "", "1", nil, 1))

	collected := collectMetric(t, reader, "no_labels_total")

	sum, ok := collected.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 1)
	assert.Equal(t, 0, sum.DataPoints[0].Attributes.Len())
}
