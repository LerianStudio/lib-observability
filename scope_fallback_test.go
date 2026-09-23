//go:build unit

package observability

import (
	"context"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/internal/buildmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestFallbackTracerIsNotLibraryScoped covers the degraded path: context
// extraction found no tracer, so the caller is service code running outside an
// instrumented request. The span it opens belongs to the service, so it must
// not be signed with this library's module scope and version - that would
// mislabel the producer and restart the series on every library release.
//
// Sequential on purpose: it swaps the process-global TracerProvider.
func TestFallbackTracerIsNotLibraryScoped(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = tp.Shutdown(context.Background())
	})

	_, span := resolveTracer(nil).Start(context.Background(), "resolve-tracer-fallback")
	span.End()

	_, span = newDefaultTrackingComponents().Tracer.Start(context.Background(), "default-components-fallback")
	span.End()

	libName, _ := buildmeta.Scope()
	spans := exporter.GetSpans()
	require.Len(t, spans, 2)

	for _, s := range spans {
		assert.Equal(t, fallbackScopeName, s.InstrumentationScope.Name, "span %q", s.Name)
		assert.NotEqual(t, libName, s.InstrumentationScope.Name,
			"a span the service opens must never be attributed to this library")
		assert.Empty(t, s.InstrumentationScope.Version, "span %q carries no library version", s.Name)
	}
}
