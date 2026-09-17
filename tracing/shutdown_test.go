//go:build unit

package tracing

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// errAlreadyShutdown mirrors what the real OTLP gRPC exporters answer when they
// are shut down a second time - otlpmetricgrpc returns "gRPC exporter is
// shutdown" - so a double drain shows up here exactly as it shows up in a
// service's logs.
var errAlreadyShutdown = errors.New("exporter is shutdown")

type countingSpanExporter struct{ shutdowns atomic.Int32 }

func (e *countingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *countingSpanExporter) Shutdown(context.Context) error {
	if e.shutdowns.Add(1) > 1 {
		return errAlreadyShutdown
	}

	return nil
}

type countingMetricExporter struct{ shutdowns atomic.Int32 }

func (e *countingMetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (e *countingMetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

func (e *countingMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return nil
}

func (e *countingMetricExporter) ForceFlush(context.Context) error { return nil }

func (e *countingMetricExporter) Shutdown(context.Context) error {
	if e.shutdowns.Add(1) > 1 {
		return errAlreadyShutdown
	}

	return nil
}

type countingLogExporter struct{ shutdowns atomic.Int32 }

func (e *countingLogExporter) Export(context.Context, []sdklog.Record) error { return nil }

func (e *countingLogExporter) ForceFlush(context.Context) error { return nil }

func (e *countingLogExporter) Shutdown(context.Context) error {
	if e.shutdowns.Add(1) > 1 {
		return errAlreadyShutdown
	}

	return nil
}

// telemetryOverCountingExporters wires the real providers over exporters that
// count their own shutdowns, through the same path NewTelemetry uses once the
// exporters exist.
func telemetryOverCountingExporters(t *testing.T) (
	*Telemetry, *countingSpanExporter, *countingMetricExporter, *countingLogExporter,
) {
	t.Helper()

	tExp := &countingSpanExporter{}
	mExp := &countingMetricExporter{}
	lExp := &countingLogExporter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tl, err := buildTelemetry(ctx, TelemetryConfig{
		LibraryName: "shutdown-test",
		ServiceName: "shutdown-test",
		Logger:      log.NewNop(),
		Redactor:    NewDefaultRedactor(),
	}, telemetryOptions{}, tExp, mExp, lExp)
	require.NoError(t, err)
	require.NotNil(t, tl)

	return tl, tExp, mExp, lExp
}

func shutdownContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// A clean exit must drain silently. Each provider already shuts the exporter it
// owns down, so listing the exporters alongside the providers drains each one
// twice and the second answer ("exporter is shutdown") is reported to the
// caller as a drain failure on every normal shutdown.
func TestShutdownTelemetry_DrainsEachOwnedExporterExactlyOnce(t *testing.T) {
	t.Parallel()

	tl, tExp, mExp, lExp := telemetryOverCountingExporters(t)

	require.NoError(t, tl.ShutdownTelemetryWithContext(shutdownContext(t)),
		"a clean shutdown must not report an error")

	assert.Equal(t, int32(1), tExp.shutdowns.Load(), "span exporter shutdowns")
	assert.Equal(t, int32(1), mExp.shutdowns.Load(), "metric exporter shutdowns")
	assert.Equal(t, int32(1), lExp.shutdowns.Load(), "log exporter shutdowns")
}

// Shutdown is not idempotent and never was: the metrics SDK answers a repeated
// reader shutdown with ErrReaderShutdown. What must not happen is the exporters
// being drained again underneath it.
func TestShutdownTelemetry_SecondCallLeavesExportersAlone(t *testing.T) {
	t.Parallel()

	tl, tExp, mExp, lExp := telemetryOverCountingExporters(t)

	require.NoError(t, tl.ShutdownTelemetryWithContext(shutdownContext(t)))

	err := tl.ShutdownTelemetryWithContext(shutdownContext(t))
	require.ErrorIs(t, err, sdkmetric.ErrReaderShutdown,
		"a repeated shutdown still reports the metrics SDK's reader error")
	assert.NotErrorIs(t, err, errAlreadyShutdown,
		"a repeated shutdown must not reach the exporters again")

	assert.Equal(t, int32(1), tExp.shutdowns.Load(), "span exporter shutdowns")
	assert.Equal(t, int32(1), mExp.shutdowns.Load(), "metric exporter shutdowns")
	assert.Equal(t, int32(1), lExp.shutdowns.Load(), "log exporter shutdowns")
}
