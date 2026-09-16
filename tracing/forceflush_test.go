//go:build unit

package tracing

import (
	"context"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestForceFlush_DrainsBufferedSpansWithoutShuttingDown(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tl := &Telemetry{TracerProvider: tp}
	tracer := tp.Tracer("forceflush-test")

	_, span := tracer.Start(context.Background(), "first")
	span.End()

	require.Empty(t, exp.GetSpans(), "the batcher must still be holding the span before the flush")
	require.NoError(t, tl.ForceFlush(context.Background()))
	require.Len(t, exp.GetSpans(), 1, "ForceFlush must push the ended span to the exporter")

	// The providers stay usable: ForceFlush is not a shutdown.
	_, second := tracer.Start(context.Background(), "second")
	second.End()

	require.NoError(t, tl.ForceFlush(context.Background()))
	assert.Len(t, exp.GetSpans(), 2, "the provider must keep working after a flush")
}

func TestForceFlush_AllThreeProviders(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()

	tl := &Telemetry{
		TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp)),
		MeterProvider:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader())),
		LoggerProvider: sdklog.NewLoggerProvider(),
	}
	t.Cleanup(func() { _ = tl.ShutdownTelemetryWithContext(context.Background()) })

	assert.NoError(t, tl.ForceFlush(context.Background()))
}

func TestForceFlush_NilReceiver(t *testing.T) {
	var tl *Telemetry

	assert.NoError(t, tl.ForceFlush(context.Background()))
}

func TestForceFlush_NilProvidersAreSkipped(t *testing.T) {
	assert.NoError(t, (&Telemetry{}).ForceFlush(context.Background()))
}

func TestForceFlush_NoopTelemetry(t *testing.T) {
	tl, err := newNoopTelemetry(TelemetryConfig{Logger: log.NewNop()})
	require.NoError(t, err)
	t.Cleanup(tl.ShutdownTelemetry)

	assert.NoError(t, tl.ForceFlush(context.Background()))
}

func TestForceFlush_TelemetryDisabledReturnsNil(t *testing.T) {
	tl, err := NewTelemetry(TelemetryConfig{Logger: log.NewNop()})
	require.NoError(t, err)
	t.Cleanup(tl.ShutdownTelemetry)

	assert.NoError(t, tl.ForceFlush(context.Background()))
}
