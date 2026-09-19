//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	observability "github.com/LerianStudio/lib-observability/v4"
	"github.com/LerianStudio/lib-observability/v4/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// tracingOnlyRoute is the single route every tracing-only test serves, so the
// expected span name and http.route are the same across the file.
const tracingOnlyRoute = "/api/ping"

// requestIDSpanAttr is the correlation-id attribute WithTelemetry publishes on
// every server span. Its value is a fresh UUID per request.
const requestIDSpanAttr = "app.request.request_id"

// appOwnCounterMetric is the instrument an application handler records through
// the metrics factory it takes off the request context. It is deliberately not
// one of the middleware's own metric names: the tracing-only mode silences the
// middleware, never the application.
const appOwnCounterMetric = "app.own.counter"

// collectedMetricNames returns every metric name the reader collected, across
// every scope. The tracing-only contract is "this middleware emits no metric
// at all", so the tests assert on the whole ResourceMetrics rather than on the
// absence of one named instrument.
func collectedMetricNames(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	names := make([]string, 0)

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}

	return names
}

// serveOneTracingRequestWith registers the caller's middleware on a fresh Fiber
// app wired to a real TracerProvider AND a real MeterProvider, mounts handler
// on tracingOnlyRoute, serves one GET against path, and returns the exported
// spans plus every metric name the ManualReader collected. wantStatus is
// required of the response, so each caller also proves which outcome it drove.
//
// register receives the Telemetry itself, so a test that needs a degraded
// dependency (a nil MetricsFactory, say) can blank it out before the handler is
// built from it.
func serveOneTracingRequestWith(
	t *testing.T,
	path string,
	handler fiber.Handler,
	wantStatus int,
	register func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry),
) ([]tracetest.SpanStub, []string) {
	t.Helper()

	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	register(app, NewTelemetryMiddleware(tel), tel)
	app.Get(tracingOnlyRoute, handler)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, wantStatus, resp.StatusCode)

	return spanExp.GetSpans(), collectedMetricNames(t, reader)
}

// serveOneTracingRequest is serveOneTracingRequestWith over a handler that
// answers 200. That status can only come from the handler, so requiring it also
// proves the handler ran.
func serveOneTracingRequest(
	t *testing.T,
	path string,
	register func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry),
) ([]tracetest.SpanStub, []string) {
	t.Helper()

	return serveOneTracingRequestWith(t, path,
		func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) },
		http.StatusOK, register)
}

// spanAttrPairs flattens a span's attributes into comparable key=value strings
// so two spans can be asserted equal without depending on attribute ordering.
// The request id is skipped: it is a fresh UUID per request by design, so its
// value can never match across two requests. Its presence is asserted
// separately.
func spanAttrPairs(span tracetest.SpanStub) []string {
	pairs := make([]string, 0, len(span.Attributes))

	for _, kv := range span.Attributes {
		if string(kv.Key) == requestIDSpanAttr {
			continue
		}

		pairs = append(pairs, string(kv.Key)+"="+kv.Value.Emit())
	}

	return pairs
}

// TestWithTracingOnly_RecordsTheSpanAndNoMetric is the mode split proven both
// ways in one test: the very same request produces an identical server span
// under WithTracingOnly and under WithTelemetry, but WithTracingOnly leaves the
// ManualReader completely empty while WithTelemetry records the standard
// http.server.request.duration histogram. The Telemetry both share HAS a
// MeterProvider and a MetricsFactory, so metric emission is switched off by the
// handler's mode, not by a nil dependency.
func TestWithTracingOnly_RecordsTheSpanAndNoMetric(t *testing.T) {
	t.Parallel()

	tracingSpans, tracingMetrics := serveOneTracingRequest(t, tracingOnlyRoute,
		func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTracingOnly(tel))
		})

	standardSpans, standardMetrics := serveOneTracingRequest(t, tracingOnlyRoute,
		func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTelemetry(tel))
		})

	require.Len(t, tracingSpans, 1, "tracing-only must still produce the server span")
	require.Len(t, standardSpans, 1)

	assert.Equal(t, http.MethodGet+" "+tracingOnlyRoute, tracingSpans[0].Name)
	assert.Equal(t, standardSpans[0].Name, tracingSpans[0].Name,
		"tracing-only must name the span exactly like WithTelemetry")
	assert.ElementsMatch(t, spanAttrPairs(standardSpans[0]), spanAttrPairs(tracingSpans[0]),
		"tracing-only must carry the same span attributes as WithTelemetry")
	assert.Equal(t, tracingOnlyRoute, getSpanAttr(tracingSpans[0], "http.route"))
	assert.NotEmpty(t, getSpanAttr(tracingSpans[0], requestIDSpanAttr),
		"tracing-only must still publish the request-id span attribute")

	assert.Empty(t, tracingMetrics,
		"WithTracingOnly must record no metric of any name; collected: %v", tracingMetrics)
	assert.Contains(t, standardMetrics, httpServerRequestDurationMetric,
		"WithTelemetry must keep recording the standard duration histogram")
}

// TestWithTracingOnly_KeepsTheMetricFactoryOnTheRequestContext proves the half
// of the contract the mode does NOT silence: the application's own metrics. The
// handler takes the factory off the request context exactly as application code
// does and records one counter, which must reach the very reader the
// middleware's own instruments are barred from.
//
// Dropping the ContextWithMetricFactory install would otherwise be silent:
// NewTrackingFromContext never returns a nil factory, it substitutes a fail-safe
// default bound to the global (no-op) meter, so the application would keep
// compiling, keep running, and quietly stop exporting.
func TestWithTracingOnly_KeepsTheMetricFactoryOnTheRequestContext(t *testing.T) {
	t.Parallel()

	// Buffered channel rather than a captured variable: the handler runs on
	// Fiber's serving goroutine, and the channel is what orders that write
	// before the assertion below reads it.
	recordErr := make(chan error, 1)

	spans, metricNames := serveOneTracingRequestWith(t, tracingOnlyRoute,
		func(c fiber.Ctx) error {
			_, _, _, factory := observability.NewTrackingFromContext(c.Context())
			recordErr <- factory.AddCounter(c.Context(), appOwnCounterMetric,
				"counter recorded by the application's own handler", "1", nil, 1)

			return c.SendStatus(http.StatusOK)
		},
		http.StatusOK,
		func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTracingOnly(tel))
		})

	require.NoError(t, <-recordErr)
	require.Len(t, spans, 1)

	assert.Contains(t, metricNames, appOwnCounterMetric,
		"the application's own counter must reach the MeterProvider the Telemetry carries")
	assert.NotContains(t, metricNames, httpServerRequestDurationMetric)
	assert.NotContains(t, metricNames, httpServerActiveRequestsMetric)
}

// TestWithTracingOnly_NeverStartsTheHostMetricsCollector proves the collector
// skip through the span, the only observable the collector leaves behind
// synchronously: it is a process-wide singleton whose first sample runs in a
// goroutine, so asserting on emitted host gauges would be both racy and
// order-dependent.
//
// The lever is a Telemetry with a real TracerProvider and MeterProvider but NO
// MetricsFactory: EnsureMetricsCollector refuses to start without one, standard
// mode stamps that refusal on the server span (status Error plus the recorded
// error event), and tracing-only never asks, so its span stays Unset with no
// events.
func TestWithTracingOnly_NeverStartsTheHostMetricsCollector(t *testing.T) {
	// Deliberately NOT parallel: this test resets telemetrycore's process-wide
	// metrics-collector singleton (metricsCollectorStarted, via
	// StopMetricsCollector), which every test in this package shares.
	StopMetricsCollector()
	t.Cleanup(StopMetricsCollector)

	spanFor := func(use func(*TelemetryMiddleware, *tracing.Telemetry) fiber.Handler) tracetest.SpanStub {
		spans, _ := serveOneTracingRequest(t, tracingOnlyRoute,
			func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
				tel.MetricsFactory = nil
				app.Use(use(mid, tel))
			})
		require.Len(t, spans, 1)

		return spans[0]
	}

	standard := spanFor(func(mid *TelemetryMiddleware, tel *tracing.Telemetry) fiber.Handler {
		return mid.WithTelemetry(tel)
	})

	tracingOnly := spanFor(func(mid *TelemetryMiddleware, tel *tracing.Telemetry) fiber.Handler {
		return mid.WithTracingOnly(tel)
	})

	require.Equal(t, codes.Error, standard.Status.Code,
		"standard mode must reach the collector and record its refusal on the span")
	require.NotEmpty(t, standard.Events,
		"the collector refusal is recorded as a span event")

	assert.Equal(t, codes.Unset, tracingOnly.Status.Code,
		"WithTracingOnly must never reach the host-metrics collector")
	assert.Empty(t, tracingOnly.Events,
		"a tracing-only span carries no collector event; got %v", tracingOnly.Events)
}

// TestWithTracingOnly_SpanMatchesWithTelemetryOnFailures extends the 200-only
// parity to the outcomes an operator actually pages on: an unmatched route, a
// handler that writes 500, and a handler that returns an error. For each, the
// tracing-only span must be indistinguishable from the one WithTelemetry
// produces for the identical request, and must still carry no metric.
func TestWithTracingOnly_SpanMatchesWithTelemetryOnFailures(t *testing.T) {
	t.Parallel()

	respondOK := func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) }

	tests := []struct {
		name       string
		path       string
		handler    fiber.Handler
		wantStatus int
	}{
		{
			name:       "unmatched route",
			path:       "/api/absent",
			handler:    respondOK,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "handler writes 500",
			path: tracingOnlyRoute,
			handler: func(c fiber.Ctx) error {
				return c.SendStatus(http.StatusInternalServerError)
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "handler returns an error",
			path: tracingOnlyRoute,
			handler: func(_ fiber.Ctx) error {
				return errors.New("handler blew up")
			},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracingSpans, tracingMetrics := serveOneTracingRequestWith(t, tt.path, tt.handler, tt.wantStatus,
				func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
					app.Use(mid.WithTracingOnly(tel))
				})

			standardSpans, _ := serveOneTracingRequestWith(t, tt.path, tt.handler, tt.wantStatus,
				func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
					app.Use(mid.WithTelemetry(tel))
				})

			require.Len(t, tracingSpans, 1)
			require.Len(t, standardSpans, 1)

			assert.Equal(t, standardSpans[0].Name, tracingSpans[0].Name)
			assert.Equal(t, standardSpans[0].Status.Code, tracingSpans[0].Status.Code)
			assert.ElementsMatch(t, spanAttrPairs(standardSpans[0]), spanAttrPairs(tracingSpans[0]))
			assert.Equal(t, strconv.Itoa(tt.wantStatus),
				getSpanAttr(tracingSpans[0], "http.response.status_code"))
			assert.Equal(t, getSpanAttr(standardSpans[0], "error.type"),
				getSpanAttr(tracingSpans[0], "error.type"))
			assert.Empty(t, tracingMetrics,
				"WithTracingOnly must record no metric of any name; collected: %v", tracingMetrics)
		})
	}
}

// TestWithTracingOnly_ExcludedRouteRecordsNothing proves the excluded-route
// short circuit is intact: no span, no metric, and the handler still answers.
func TestWithTracingOnly_ExcludedRouteRecordsNothing(t *testing.T) {
	t.Parallel()

	spans, metricNames := serveOneTracingRequest(t, tracingOnlyRoute,
		func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTracingOnly(tel, tracingOnlyRoute))
		})

	assert.Empty(t, spans, "an excluded route must produce no span")
	assert.Empty(t, metricNames, "an excluded route must produce no metric")
}

// TestWithTracingOnly_EndTracingSpansKeepsTheOwnedSpan mirrors the owned-flag
// contract for the tracing-only handler: with EndTracingSpans also registered,
// the span is still ended exactly once and still carries the post-c.Next
// finalization (route-template name and http.route), which a premature end by
// EndTracingSpans would silently discard.
func TestWithTracingOnly_EndTracingSpansKeepsTheOwnedSpan(t *testing.T) {
	t.Parallel()

	spans, metricNames := serveOneTracingRequest(t, tracingOnlyRoute,
		func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry) {
			app.Use(mid.WithTracingOnly(tel))
			app.Use(mid.EndTracingSpans)
		})

	require.Len(t, spans, 1, "the owned span must be ended exactly once")
	assert.Equal(t, http.MethodGet+" "+tracingOnlyRoute, spans[0].Name)
	assert.Equal(t, tracingOnlyRoute, getSpanAttr(spans[0], "http.route"))
	assert.Empty(t, metricNames, "WithTracingOnly must record no metric of any name")
}
