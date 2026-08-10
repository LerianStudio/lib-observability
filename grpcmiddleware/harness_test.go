//go:build unit

package grpcmiddleware

import (
	"context"
	"testing"

	"github.com/LerianStudio/lib-observability/v3/metrics"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newMetricsHarness wires a real OTel SDK ManualReader so tests can assert on
// the rpc.*.duration histograms exactly as they would appear to an exporter.
// Returns the configured Telemetry pointer plus the reader.
func newMetricsHarness(t *testing.T) (*tracing.Telemetry, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	factory, err := metrics.NewMetricsFactory(mp.Meter("test-library"), nil)
	require.NoError(t, err)

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{
			LibraryName:     "test-library",
			EnableTelemetry: true,
		},
		MeterProvider:  mp,
		MetricsFactory: factory,
	}

	return tel, reader
}

// newTelemetryHarness extends newMetricsHarness with a real TracerProvider
// backed by an InMemoryExporter so tests can assert on both the duration
// histograms and the span attributes produced by the interceptors.
func newTelemetryHarness(
	t *testing.T,
) (*tracing.Telemetry, *sdkmetric.ManualReader, *tracetest.InMemoryExporter) {
	t.Helper()

	tel, reader := newMetricsHarness(t)

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tel.TracerProvider = tp

	return tel, reader, spanExp
}

// setupTestTracer sets up a test tracer provider and returns it along with a span recorder.
func setupTestTracer(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spanRecorder),
	)

	oldPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTextMapPropagator(oldPropagator)
	})

	return tracerProvider, spanRecorder
}

func attrValue(set attribute.Set, key string) (string, bool) {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return "", false
	}

	return v.AsString(), true
}
