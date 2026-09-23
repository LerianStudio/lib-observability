//go:build unit

package tracing

import (
	"context"
	"sync"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/internal/buildmeta"
	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// scopeCapturingExporter records the instrumentation scope of everything the
// MeterProvider exports, so a test can name the scope a given instrument was
// created on.
type scopeCapturingExporter struct {
	mu     sync.Mutex
	scopes map[string]instrumentationScope // metric name -> scope
}

type instrumentationScope struct{ name, version string }

func (*scopeCapturingExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (*scopeCapturingExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

func (e *scopeCapturingExporter) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.scopes == nil {
		e.scopes = make(map[string]instrumentationScope)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			e.scopes[m.Name] = instrumentationScope{sm.Scope.Name, sm.Scope.Version}
		}
	}

	return nil
}

func (*scopeCapturingExporter) ForceFlush(context.Context) error { return nil }
func (*scopeCapturingExporter) Shutdown(context.Context) error   { return nil }

func (e *scopeCapturingExporter) scopeOf(name string) (instrumentationScope, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	scope, ok := e.scopes[name]

	return scope, ok
}

// TestMetricsFactoryCarriesServiceScope pins the scope of the factory that
// NewTelemetry hands the service on Telemetry.MetricsFactory and on the request
// context. It is the documented place a service declares its own business
// metrics, so the instruments must keep the service's LibraryName. Stamping
// them with the library's scope would restart every one of those Prometheus
// series on a library upgrade the service had no part in.
func TestMetricsFactoryCarriesServiceScope(t *testing.T) {
	t.Parallel()

	libName, libVersion := buildmeta.Scope()

	tests := []struct {
		name        string
		libraryName string
		serviceName string
		wantScope   string
	}{
		{
			name:        "library name set",
			libraryName: "github.com/LerianStudio/midaz/v4/components/ledger",
			serviceName: "midaz-ledger",
			wantScope:   "github.com/LerianStudio/midaz/v4/components/ledger",
		},
		{
			name:        "library name blank, service name set",
			serviceName: "midaz-ledger",
			wantScope:   "",
		},
		{
			// Nothing names the service, so its metric lands on the empty
			// scope - and must still be told apart from the library's.
			name:      "library name and service name blank",
			wantScope: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mExp := &scopeCapturingExporter{}

			tl, err := buildTelemetry(context.Background(), TelemetryConfig{
				LibraryName: tt.libraryName,
				ServiceName: tt.serviceName,
				Logger:      log.NewNop(),
				Redactor:    NewDefaultRedactor(),
			}, telemetryOptions{}, &countingSpanExporter{}, mExp, &countingLogExporter{})
			require.NoError(t, err)

			t.Cleanup(func() { _ = tl.ShutdownTelemetryWithContext(context.Background()) })

			counter, err := tl.MetricsFactory.Counter(metrics.Metric{
				Name:        "settlements_completed",
				Description: "a metric the service declares, not the library",
			})
			require.NoError(t, err)
			counter.Add(context.Background(), 1)

			// A library instrument on the same provider, created the way the
			// middleware creates its transport instruments.
			libCounter, err := buildmeta.Meter(tl.MeterProvider).Int64Counter("library_instrument")
			require.NoError(t, err)
			libCounter.Add(context.Background(), 1)

			require.NoError(t, tl.ForceFlush(context.Background()))

			svc, ok := mExp.scopeOf("settlements_completed")
			require.True(t, ok, "the service metric must have been exported")
			assert.Equal(t, tt.wantScope, svc.name, "a service business metric keeps the service scope")
			assert.Empty(t, svc.version, "a service business metric carries no library version")

			lib, ok := mExp.scopeOf("library_instrument")
			require.True(t, ok, "the library instrument must have been exported")
			assert.Equal(t, libName, lib.name)
			assert.Equal(t, libVersion, lib.version)
			assert.NotEqual(t, lib, svc, "a service metric and a library instrument must be distinguishable")
		})
	}
}

// TestServiceTracerWithoutProvider pins the degraded path: the middleware puts
// ServiceTracer on the request context, so a handler calling Start on it must
// never hit a nil tracer.
func TestServiceTracerWithoutProvider(t *testing.T) {
	t.Parallel()

	for name, tl := range map[string]*Telemetry{
		"nil telemetry": nil,
		"no provider":   {TelemetryConfig: TelemetryConfig{ServiceName: "ledger"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tracer := tl.ServiceTracer()
			require.NotNil(t, tracer)

			_, span := tracer.Start(context.Background(), "handler-span")
			assert.False(t, span.SpanContext().IsValid(), "a noop tracer records nothing")
			span.End()
		})
	}
}
