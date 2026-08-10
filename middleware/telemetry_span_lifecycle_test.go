//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// telemetryTestApp wires a fresh Fiber app + TelemetryMiddleware against a
// span recorder, applying the caller-provided route/middleware registration.
// It returns the app (to drive requests) and the span recorder (to assert on
// ended spans).
func telemetryTestApp(
	t *testing.T,
	register func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry),
) (*fiber.App, *tracetest.SpanRecorder) {
	t.Helper()

	tp, spanRecorder := setupTestTracer(t)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(oldTP) })

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{
			LibraryName:     "test-library",
			EnableTelemetry: true,
		},
		TracerProvider: tp,
	}

	mid := NewTelemetryMiddleware(tel)

	app := fiber.New()
	register(app, mid, tel)

	return app, spanRecorder
}

// findSpan returns the ended span with the exact name, plus a found flag.
func findSpan(spans []sdktrace.ReadOnlySpan, name string) (sdktrace.ReadOnlySpan, bool) {
	for _, s := range spans {
		if s.Name() == name {
			return s, true
		}
	}

	return nil, false
}

// spanAttrString returns the string value of the named attribute on the span,
// and whether it was present.
func spanAttrString(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}

	return "", false
}

// spanNames collects ended span names for diagnostic assertion messages.
func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}

	return names
}

// assertRootSpanEndedOnce asserts that exactly one span carrying the expected
// template-based name was ended exactly once and that http.route is present.
// This is the shared lifecycle+naming contract across all middleware orderings.
func assertRootSpanEndedOnce(
	t *testing.T,
	spanRecorder *tracetest.SpanRecorder,
	expectedName, expectedRoute string,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		return len(spanRecorder.Ended()) >= 1
	}, time.Second, 5*time.Millisecond, "expected the root span to be ended")

	spans := spanRecorder.Ended()

	matches := 0
	for _, s := range spans {
		if s.Name() == expectedName {
			matches++
		}
	}
	assert.Equal(t, 1, matches,
		"expected exactly one span named %q ended exactly once; got %v",
		expectedName, spanNames(spans))

	s, ok := findSpan(spans, expectedName)
	require.True(t, ok, "span %q not found among %v", expectedName, spanNames(spans))

	route, present := spanAttrString(s, "http.route")
	assert.True(t, present, "http.route must be present on the finalized span")
	assert.Equal(t, expectedRoute, route)
}

// TestSpanLifecycle_MiddlewareOrderings proves the fix is robust to the order
// in which the consumer registers WithTelemetry / EndTracingSpans: in every
// ordering the span is named by the route template, carries http.route, and is
// ended exactly once (no double-end, no lost finalization). Before the fix,
// EndTracingSpans unwinding first (Fiber LIFO) ended the span before
// applyTelemetrySpanAttributes ran, silently dropping the rename and http.route.
func TestSpanLifecycle_MiddlewareOrderings(t *testing.T) {
	const (
		route        = "/api/users/:id"
		concretePath = "/api/users/42" // numeric ID must never reach the name
		wantName     = "GET /api/users/:id"
	)

	handler := func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) }

	t.Run("WithTelemetry first, EndTracingSpans last (standard order)", func(t *testing.T) {
		app, rec := telemetryTestApp(t, func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTelemetry(tel))
			// EndTracingSpans as the last (innermost) handler unwinds FIRST
			// under LIFO — the exact hazard the owned flag neutralizes.
			app.Get(route, mid.EndTracingSpans, handler)
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, concretePath, nil))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		assertRootSpanEndedOnce(t, rec, wantName, route)
	})

	t.Run("EndTracingSpans registered before the route (plugin order)", func(t *testing.T) {
		app, rec := telemetryTestApp(t, func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTelemetry(tel))
			app.Use(mid.EndTracingSpans)
			app.Get(route, handler)
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, concretePath, nil))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		assertRootSpanEndedOnce(t, rec, wantName, route)
	})

	t.Run("no EndTracingSpans registered at all", func(t *testing.T) {
		app, rec := telemetryTestApp(t, func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTelemetry(tel))
			app.Get(route, handler)
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, concretePath, nil))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// WithTelemetry is self-sufficient: it ends its own span even with no
		// EndTracingSpans in the chain.
		assertRootSpanEndedOnce(t, rec, wantName, route)
	})

	t.Run("EndTracingSpans registered before WithTelemetry (inverted order)", func(t *testing.T) {
		app, rec := telemetryTestApp(t, func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			// Pathological order: EndTracingSpans is the OUTERMOST middleware, so
			// it unwinds LAST — after WithTelemetry has finalized and ended its
			// own owned span. EndTracingSpans must find the owned state and skip
			// it (no double-end, no lost finalization).
			app.Use(mid.EndTracingSpans)
			app.Use(mid.WithTelemetry(tel))
			app.Get(route, handler)
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, concretePath, nil))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		assertRootSpanEndedOnce(t, rec, wantName, route)
	})
}

// TestWithTelemetry_SpanNameUsesRouteTemplate is the primary anti-cardinality /
// anti-PII contract: two requests to the same :param route with different
// concrete identifiers MUST share one span name (the template), and the
// concrete identifiers MUST NOT appear anywhere in the span name.
func TestWithTelemetry_SpanNameUsesRouteTemplate(t *testing.T) {
	const route = "/v1/dict/statistics/keys/:key"

	app, rec := telemetryTestApp(t, func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
		app.Use(mid.WithTelemetry(tel))
		app.Get(route, func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	})

	// Two distinct concrete keys, including a numeric one, that must be masked
	// by the template.
	for _, key := range []string{"12345678901", "a1b2c3d4-e5f6-7890-abcd-ef1234567890"} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/dict/statistics/keys/"+key, nil))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}

	require.Eventually(t, func() bool {
		return len(rec.Ended()) == 2
	}, time.Second, 5*time.Millisecond)

	spans := rec.Ended()
	require.Len(t, spans, 2)

	for _, s := range spans {
		assert.Equal(t, "GET "+route, s.Name(),
			"both requests must collapse to the single template name")
		assert.NotContains(t, s.Name(), "12345678901",
			"numeric identifier must never appear in the span name")
		assert.NotContains(t, s.Name(), "a1b2c3d4",
			"UUID identifier must never appear in the span name")

		gotRoute, present := spanAttrString(s, "http.route")
		assert.True(t, present, "http.route must remain available as an attribute")
		assert.Equal(t, route, gotRoute)
	}
}

// TestWithTelemetry_CreationSpanNameIsMethodOnly asserts the PII-free-at-creation
// guarantee (the name the sampler sees): the span is CREATED with a method-only
// name, so a concrete numeric identifier in the path is absent from the name
// even before routing renames it. We observe the creation name via the active
// span inside the handler, which runs after Start() but before the post-c.Next
// rename in applyTelemetrySpanAttributes.
func TestWithTelemetry_CreationSpanNameIsMethodOnly(t *testing.T) {
	var creationName string

	app, _ := telemetryTestApp(t, func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
		app.Use(mid.WithTelemetry(tel))
		app.Get("/accounts/:id", func(c fiber.Ctx) error {
			// The span exists (created by WithTelemetry) but has not yet been
			// renamed — the rename happens after this handler returns. A
			// ReadWriteSpan exposes its current Name().
			if rw, ok := oteltraceSpanFromCtx(c.Context()); ok {
				creationName = rw.Name()
			}

			return c.SendStatus(http.StatusOK)
		})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/accounts/987654321", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "GET", creationName,
		"creation-time span name must be method-only (PII-free for the sampler)")
	assert.NotContains(t, creationName, "987654321",
		"concrete numeric identifier must never be present at creation time")
}

// oteltraceSpanFromCtx returns the active span from the context as an SDK
// ReadWriteSpan so tests can read its current (pre-rename) name.
func oteltraceSpanFromCtx(ctx context.Context) (sdktrace.ReadWriteSpan, bool) {
	s := oteltrace.SpanFromContext(ctx)
	rw, ok := s.(sdktrace.ReadWriteSpan)

	return rw, ok
}
