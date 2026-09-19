//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// serveOneTracingRequest registers the caller's middleware on a fresh Fiber app
// wired to a real TracerProvider AND a real MeterProvider, serves one GET
// request against the given path, and returns the exported spans plus every
// metric name the ManualReader collected. The 200 it requires can only come
// from the handler, so it also proves the handler ran.
func serveOneTracingRequest(
	t *testing.T,
	path string,
	register func(app *fiber.App, mid *TelemetryMiddleware, tel *tracing.Telemetry),
) ([]tracetest.SpanStub, []string) {
	t.Helper()

	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	register(app, NewTelemetryMiddleware(tel), tel)
	app.Get(tracingOnlyRoute, func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, http.StatusOK, resp.StatusCode, "the handler must still run")

	return spanExp.GetSpans(), collectedMetricNames(t, reader)
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

// TestWithTracingOnly_ExcludedRouteRecordsNothing proves the excluded-route
// short circuit is intact: no span, no metric, and the handler still answers.
func TestWithTracingOnly_ExcludedRouteRecordsNothing(t *testing.T) {
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
