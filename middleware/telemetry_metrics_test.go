//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	observability "github.com/LerianStudio/lib-observability/v3"
	constant "github.com/LerianStudio/lib-observability/v3/constants"
	"github.com/LerianStudio/lib-observability/v3/metrics"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// countingError is a perfectly stringifiable error that counts its Error()
// calls, so tests can assert the message reached a sink via exactly one call.
type countingError struct {
	calls *atomic.Int32
}

func (e countingError) Error() string {
	e.calls.Add(1)

	return "handler detail"
}

// newMetricsHarness wires a real OTel SDK ManualReader so tests can assert on
// the http.server.request.duration histogram exactly as it would appear to an
// exporter. Returns the configured Telemetry pointer plus a flush function.
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

// findDurationHistogram extracts the http.server.request.duration histogram
// data point from a ManualReader collection. Returns nil if the metric is
// absent (which the tests use to assert non-recording paths).
func findDurationHistogram(
	t *testing.T,
	reader *sdkmetric.ManualReader,
) *metricdata.HistogramDataPoint[float64] {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != httpServerRequestDurationMetric {
				continue
			}

			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected Float64 histogram for %s, got %T", m.Name, m.Data)
			require.NotEmpty(t, h.DataPoints, "histogram has no data points")
			require.Equal(t, "s", m.Unit, "metric unit must be seconds")

			dp := h.DataPoints[0]
			return &dp
		}
	}

	return nil
}

func attrValue(set attribute.Set, key string) (string, bool) {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return "", false
	}

	return v.AsString(), true
}

// newTelemetryHarness extends newMetricsHarness with a real TracerProvider
// backed by an InMemoryExporter so tests can assert on both the
// http.server.request.duration histogram and the span attributes produced
// by WithTelemetry in a single fixture.
func newTelemetryHarness(
	t *testing.T,
) (*tracing.Telemetry, *sdkmetric.ManualReader, *tracetest.InMemoryExporter) {
	t.Helper()

	tel, reader := newMetricsHarness(t)

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(tracing.AttrBagSpanProcessor{}),
		sdktrace.WithSyncer(spanExp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tel.TracerProvider = tp

	return tel, reader, spanExp
}

// getSpanAttr returns the string value for the named attribute on the given
// stub span, or "" if absent. Non-string attribute values are also returned
// stringified to keep call-site assertions uniform.
func getSpanAttr(span tracetest.SpanStub, key string) string {
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}

	return ""
}

// TestWithTelemetry_ServerSpanExcludesIdentityFromUpstreamMiddleware guards
// the middleware-ordering contract: a tenant-resolution middleware registered
// BEFORE WithTelemetry stores identity in the request AttrBag (and OTel
// baggage), and the AttrBag span processor copies that context onto every
// span at OnStart. The built-in HTTP server span must never receive that
// identity, while downstream application spans started from the request
// context must retain it.
func TestWithTelemetry_ServerSpanExcludesIdentityFromUpstreamMiddleware(t *testing.T) {
	tel, _, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)

	// Simulates an upstream tenant-resolution middleware that publishes
	// authenticated identity for downstream application telemetry.
	app.Use(func(c fiber.Ctx) error {
		ctx := observability.ContextWithSpanAttributes(c.Context(),
			attribute.String(constant.AttrKeyTenantID, "tenant-123"),
			attribute.String(constant.AttrKeyContextID, "context-456"),
		)

		member, err := baggage.NewMember(constant.AttrKeyTenantID, "tenant-123")
		require.NoError(t, err)

		bag, err := baggage.New(member)
		require.NoError(t, err)

		c.SetContext(baggage.ContextWithBaggage(ctx, bag))

		return c.Next()
	})
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/users/:id", func(c fiber.Ctx) error {
		// Downstream application span started from the request context.
		_, appSpan := tel.TracerProvider.Tracer("app").Start(c.Context(), "app-operation")
		appSpan.End()

		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/users/42", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var serverSpan, appSpan *tracetest.SpanStub

	spans := spanExp.GetSpans()
	for i := range spans {
		switch {
		case spans[i].SpanKind == oteltrace.SpanKindServer:
			serverSpan = &spans[i]
		case spans[i].Name == "app-operation":
			appSpan = &spans[i]
		}
	}

	require.NotNil(t, serverSpan, "expected the HTTP server span to be exported")
	require.NotNil(t, appSpan, "expected the downstream application span to be exported")

	assert.Empty(t, getSpanAttr(*serverSpan, constant.AttrKeyTenantID),
		"built-in HTTP server span must not inherit tenant identity from upstream middleware")
	assert.Empty(t, getSpanAttr(*serverSpan, constant.AttrKeyContextID),
		"built-in HTTP server span must not inherit context identity from upstream middleware")
	assert.NotEmpty(t, getSpanAttr(*serverSpan, "app.request.request_id"),
		"non-identity AttrBag entries must still reach the server span")

	assert.Equal(t, "tenant-123", getSpanAttr(*appSpan, constant.AttrKeyTenantID),
		"downstream application spans must retain tenant identity")
	assert.Equal(t, "context-456", getSpanAttr(*appSpan, constant.AttrKeyContextID),
		"downstream application spans must retain context identity")
}

// TestHTTPServerDurationBuckets_MatchOTelAdvisory locks the bucket layout
// against the current OTel HTTP semconv advisory. Any change to this slice
// is observable from dashboards, so it MUST be a deliberate spec-tracking
// update — never an accidental edit.
func TestHTTPServerDurationBuckets_MatchOTelAdvisory(t *testing.T) {
	expected := []float64{
		0.005, 0.01, 0.025, 0.05, 0.075,
		0.1, 0.25, 0.5, 0.75,
		1, 2.5, 5, 7.5, 10,
	}
	assert.Equal(t, expected, httpServerDurationBuckets)
}

// TestWithTelemetry_RecordsDurationOnSuccess verifies that a successful 200
// request emits the duration histogram with the route template (not the raw
// path) and no error.type attribute.
func TestWithTelemetry_RecordsDurationOnSuccess(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/users/:id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/users/42", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp, "expected http.server.request.duration to be recorded")
	assert.EqualValues(t, 1, dp.Count, "exactly one request should have been recorded")
	assert.GreaterOrEqual(t, dp.Sum, 0.0, "duration sum must be non-negative seconds")

	method, ok := attrValue(dp.Attributes, "http.request.method")
	require.True(t, ok)
	assert.Equal(t, "GET", method)

	route, ok := attrValue(dp.Attributes, "http.route")
	require.True(t, ok)
	assert.Equal(t, "/api/users/:id", route, "must use route template, never raw path")

	statusVal, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusOK, statusVal.AsInt64())

	_, hasErr := dp.Attributes.Value(attribute.Key("error.type"))
	assert.False(t, hasErr, "error.type must not be set on a 2xx response")
}

// TestWithTelemetry_RecordsDurationOn4xx verifies a client-error response is
// recorded without an error.type attribute (4xx is not classified as an error
// per OTel HTTP server conventions).
func TestWithTelemetry_RecordsDurationOn4xx(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/items/:id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusNotFound)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/items/missing", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)
	assert.EqualValues(t, 1, dp.Count)

	statusVal, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusNotFound, statusVal.AsInt64())

	_, hasErr := dp.Attributes.Value(attribute.Key("error.type"))
	assert.False(t, hasErr, "4xx must not set error.type")
}

// TestWithTelemetry_RecordsDurationOn5xxStatus verifies that a 5xx response
// returned by the handler (without returning an error) is classified with a
// numeric error.type derived from the status code.
func TestWithTelemetry_RecordsDurationOn5xxStatus(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/health", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusServiceUnavailable)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/health", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok, "5xx without handler error must still set error.type")
	assert.Equal(t, "503", errType)

	// SendStatus(503) carries no handler error, so error.type_original must
	// be absent everywhere (regression guard against re-introducing the
	// Go-type label on the metric).
	_, hasOrigOnMetric := dp.Attributes.Value(attribute.Key("error.type_original"))
	assert.False(t, hasOrigOnMetric,
		"error.type_original must NEVER appear on the metric")
}

// TestWithTelemetry_RecordsDurationOnHandlerError verifies the Opção C
// hybrid for a generic handler-returned error reconciled to 500:
//   - Metric error.type is the status code string ("500"), never the Go type
//     name (which would balloon cardinality across application error types).
//   - Metric does NOT carry error.type_original.
//   - Span carries the same numeric error.type plus error.type_original with
//     the originating Go type name for debugging.
func TestWithTelemetry_RecordsDurationOnHandlerError(t *testing.T) {
	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			return c.Status(http.StatusInternalServerError).SendString(err.Error())
		},
	})
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	sentinel := errors.New("boom")
	app.Get("/api/explode", func(c fiber.Ctx) error {
		return sentinel
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/explode", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// Metric: numeric error.type, no error.type_original.
	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok)
	assert.Equal(t, "500", errType,
		"metric error.type must be status-driven for low cardinality")

	_, hasOrigOnMetric := dp.Attributes.Value(attribute.Key("error.type_original"))
	assert.False(t, hasOrigOnMetric,
		"error.type_original must NEVER appear on the metric")

	// Span: same numeric error.type + originating Go type name.
	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)
	assert.Equal(t, "500", getSpanAttr(spans[0], "error.type"))
	assert.Equal(t, "errors.errorString",
		getSpanAttr(spans[0], "error.type_original"),
		"span must surface the originating Go type name for debugging")
}

// findEventAttr returns the string value of the named attribute on the first
// span event matching eventName, or "" if either is absent.
func findEventAttr(spans []tracetest.SpanStub, eventName, attrKey string) (value string, found bool) {
	for _, span := range spans {
		for _, event := range span.Events {
			if event.Name != eventName {
				continue
			}

			for _, attr := range event.Attributes {
				if string(attr.Key) == attrKey {
					return attr.Value.Emit(), true
				}
			}
		}
	}

	return "", false
}

// TestWithTelemetry_5xxHandlerErrorRecordsSemconvException verifies FIX 8's
// >=500 branch: span.RecordError produces the OTel semconv "exception" event
// (exception.type/exception.message) rather than a custom-named event -
// Tempo/Jaeger and other APM backends index specifically on those semconv
// keys, so a differently-named event with the same information is invisible
// to them. tracing.ErrorMessage's sanitization (secrets stripped,
// length-capped) still applies before the message reaches the span.
func TestWithTelemetry_5xxHandlerErrorRecordsSemconvException(t *testing.T) {
	t.Parallel()

	tel, _, spanExp := newTelemetryHarness(t)
	var errorCalls atomic.Int32

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			return c.SendStatus(http.StatusInternalServerError)
		},
	})
	app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
	app.Get("/error", func(fiber.Ctx) error {
		return countingError{calls: &errorCalls}
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/error", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.EqualValues(t, 1, errorCalls.Load(),
		"the message must reach the span, which requires exactly one Error() call")

	spans := spanExp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status.Code)
	assert.Equal(t, "HTTP 500", spans[0].Status.Description)
	assert.Equal(t, "middleware.countingError", getSpanAttr(spans[0], "error.type_original"))

	msg, found := findEventAttr(spans, "exception", "exception.message")
	require.True(t, found, "span must record the OTel semconv exception event on a 5xx")
	assert.Equal(t, "handler detail", msg,
		"the exception event must carry the handler error's message")

	_, hasCustomEvent := findEventAttr(spans, "http.handler.error", "error")
	assert.False(t, hasCustomEvent, "the custom event is reserved for <500; a 5xx uses the semconv exception event instead")
}

// TestWithTelemetry_4xxHandlerErrorUsesCustomEvent verifies the <500 branch
// still uses the custom http.handler.error event (unchanged from before FIX
// 8's split), since a mapped 4xx must never produce an "exception" event or
// ERROR span status per OTel semconv.
func TestWithTelemetry_4xxHandlerErrorUsesCustomEvent(t *testing.T) {
	t.Parallel()

	tel, _, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
	app.Get("/error", func(fiber.Ctx) error {
		return fiber.NewError(http.StatusBadRequest, "bad input")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/error", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	spans := spanExp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Unset, spans[0].Status.Code, "a 4xx must never set ERROR span status")

	msg, found := findEventAttr(spans, "http.handler.error", "error")
	require.True(t, found, "a 4xx handler error must still record the custom event")
	assert.Equal(t, "bad input", msg)

	_, hasExceptionEvent := findEventAttr(spans, "exception", "exception.message")
	assert.False(t, hasExceptionEvent, "a 4xx must never record the semconv exception event")
}

// TestWithTelemetry_FiberError4xxOmitsErrorTypeOnMetric verifies that a
// handler returning fiber.NewError(4xx) records the effective HTTP status
// from the error but does NOT set error.type on the metric (4xx is not
// classified as an error per the status-driven contract). The originating
// *fiber.Error type is preserved on the span as error.type_original.
func TestWithTelemetry_FiberError4xxOmitsErrorTypeOnMetric(t *testing.T) {
	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/items/:id", func(c fiber.Ctx) error {
		return fiber.NewError(http.StatusNotFound, "not found")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/items/missing", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"client must observe the 404 from Fiber's default error handler")

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)
	assert.EqualValues(t, 1, dp.Count)

	statusVal, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusNotFound, statusVal.AsInt64(),
		"status_code must reflect the *fiber.Error code, not the unwritten default")

	// Metric MUST omit error.type and error.type_original for 4xx.
	_, hasErrTypeOnMetric := dp.Attributes.Value(attribute.Key("error.type"))
	assert.False(t, hasErrTypeOnMetric,
		"4xx must not set error.type on the metric per status-driven classification")
	_, hasOrigOnMetric := dp.Attributes.Value(attribute.Key("error.type_original"))
	assert.False(t, hasOrigOnMetric,
		"error.type_original must NEVER appear on the metric")

	// Span MUST also omit error.type but carry error.type_original.
	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)
	assert.Empty(t, getSpanAttr(spans[0], "error.type"),
		"span error.type follows the same status-driven rule")
	assert.Equal(t, "fiber.Error",
		getSpanAttr(spans[0], "error.type_original"),
		"span must preserve the originating *fiber.Error type")
}

// TestWithTelemetry_FiberError400OmitsErrorTypeOnMetric asserts the same
// contract for 4xx bad-request errors raised via fiber.NewError, independently
// from the 404 case to catch regressions for either code path.
func TestWithTelemetry_FiberError400OmitsErrorTypeOnMetric(t *testing.T) {
	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Post("/api/validate", func(c fiber.Ctx) error {
		return fiber.NewError(http.StatusBadRequest, "invalid payload")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/validate", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	statusVal, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusBadRequest, statusVal.AsInt64(),
		"status_code must reflect fiber.NewError(400)")

	_, hasErrTypeOnMetric := dp.Attributes.Value(attribute.Key("error.type"))
	assert.False(t, hasErrTypeOnMetric,
		"400 must not set error.type on the metric")
	_, hasOrigOnMetric := dp.Attributes.Value(attribute.Key("error.type_original"))
	assert.False(t, hasOrigOnMetric)

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)
	assert.Empty(t, getSpanAttr(spans[0], "error.type"))
	assert.Equal(t, "fiber.Error",
		getSpanAttr(spans[0], "error.type_original"))
}

// TestWithTelemetry_RecordsDurationOnFiberError5xx verifies that a 5xx
// fiber.NewError is reflected in status_code AND error.type on the duration
// metric using the status-driven numeric label, with the originating Go type
// preserved on the span as error.type_original (never on the metric).
func TestWithTelemetry_RecordsDurationOnFiberError5xx(t *testing.T) {
	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/down", func(c fiber.Ctx) error {
		return fiber.NewError(http.StatusBadGateway, "upstream gone")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/down", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	statusVal, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusBadGateway, statusVal.AsInt64())

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok)
	assert.Equal(t, "502", errType,
		"metric error.type must be status-driven for low cardinality")

	_, hasOrigOnMetric := dp.Attributes.Value(attribute.Key("error.type_original"))
	assert.False(t, hasOrigOnMetric,
		"error.type_original must NEVER appear on the metric")

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)
	assert.Equal(t, "502", getSpanAttr(spans[0], "error.type"))
	assert.Equal(t, "fiber.Error", getSpanAttr(spans[0], "error.type_original"))
}

// TestWithTelemetry_FiberErrorAndSendStatusAreConsistent verifies the core
// motivation of the status-driven contract: a 503 raised via
// fiber.NewError(503) and a 503 written via c.SendStatus(503) MUST produce
// the same metric time series so alert rules of the form error_type=~"5.."
// aggregate reliably across both code paths.
func TestWithTelemetry_FiberErrorAndSendStatusAreConsistent(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/raise", func(c fiber.Ctx) error {
		return fiber.NewError(http.StatusServiceUnavailable, "down")
	})
	app.Get("/api/send", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusServiceUnavailable)
	})

	r1, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/raise", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, r1.Body.Close()) }()
	require.Equal(t, http.StatusServiceUnavailable, r1.StatusCode)

	r2, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/send", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, r2.Body.Close()) }()
	require.Equal(t, http.StatusServiceUnavailable, r2.StatusCode)

	// Both requests share method+status+error.type but have different
	// http.route values, so they remain two distinct time series, each with
	// Count==1. Both MUST carry error.type="503" (status-driven) and no
	// error.type_original on the metric.
	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var points []metricdata.HistogramDataPoint[float64]

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != httpServerRequestDurationMetric {
				continue
			}

			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			points = append(points, h.DataPoints...)
		}
	}

	require.Len(t, points, 2,
		"each route is a distinct series; both must record exactly one point")

	for _, dp := range points {
		errType, ok := attrValue(dp.Attributes, "error.type")
		require.True(t, ok,
			"both fiber.NewError and SendStatus 5xx paths MUST set error.type")
		assert.Equal(t, "503", errType,
			"both paths MUST produce the same status-driven error.type label")

		_, hasOrig := dp.Attributes.Value(attribute.Key("error.type_original"))
		assert.False(t, hasOrig,
			"error.type_original must NEVER appear on the metric")

		assert.EqualValues(t, 1, dp.Count)
	}
}

// TestWithTelemetry_GenericHandlerErrorStatusCodeIs500 verifies that when a
// handler returns a non-fiber error and no custom ErrorHandler has rewritten
// the status code by the time the metric is recorded, the metric reports
// status_code=500 with the status-driven error.type="500" (not the Go type
// name). The originating Go type identity is preserved on the span via
// error.type_original.
func TestWithTelemetry_GenericHandlerErrorStatusCodeIs500(t *testing.T) {
	tel, reader, spanExp := newTelemetryHarness(t)

	// Use Fiber's default ErrorHandler (no override) so the response status
	// is materialized AFTER the WithTelemetry middleware unwinds.
	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/explode", func(c fiber.Ctx) error {
		return errors.New("boom")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/explode", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"Fiber's default error handler maps unknown errors to 500")

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	statusVal, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusInternalServerError, statusVal.AsInt64(),
		"generic handler error must record status_code=500 to match client view")

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok)
	assert.Equal(t, "500", errType)

	_, hasOrigOnMetric := dp.Attributes.Value(attribute.Key("error.type_original"))
	assert.False(t, hasOrigOnMetric)

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)
	assert.Equal(t, "500", getSpanAttr(spans[0], "error.type"))
	assert.Equal(t, "errors.errorString",
		getSpanAttr(spans[0], "error.type_original"))
}

// TestWithTelemetry_DoesNotRecordForExcludedRoute verifies that excluded
// routes bypass duration recording entirely.
func TestWithTelemetry_DoesNotRecordForExcludedRoute(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel, "/swagger"))

	app.Get("/swagger/index.html", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Nil(t, findDurationHistogram(t, reader),
		"excluded route must not record http.server.request.duration")
}

// TestWithTelemetry_NilTelemetryDoesNotRecord verifies the handler is safe and
// silent when no Telemetry is configured.
func TestWithTelemetry_NilTelemetryDoesNotRecord(t *testing.T) {
	// Standalone reader without a real Telemetry attached - we still expect
	// the metric to be absent because the middleware short-circuits.
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	app := fiber.New()
	mid := NewTelemetryMiddleware(nil)
	app.Use(mid.WithTelemetry(nil))

	app.Get("/ping", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ping", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			assert.NotEqual(t, httpServerRequestDurationMetric, m.Name,
				"nil telemetry must not record duration")
		}
	}
}

// TestWithTelemetry_NilMeterProviderDoesNotRecord verifies that recording is
// skipped when Telemetry is present but has no MeterProvider, while the
// request itself still completes successfully.
func TestWithTelemetry_NilMeterProviderDoesNotRecord(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library"},
		// MeterProvider intentionally nil.
		MetricsFactory: metrics.NewNopFactory(),
	}

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/no-mp", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/no-mp", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Nil(t, findDurationHistogram(t, reader),
		"nil MeterProvider must not record duration")
}

// TestWithTelemetry_NilMetricsFactoryDoesNotRecord verifies that recording is
// gated on MetricsFactory presence even when MeterProvider is configured.
// This matches the requirement that nil MetricsFactory must silently skip.
func TestWithTelemetry_NilMetricsFactoryDoesNotRecord(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library"},
		MeterProvider:   mp,
		// MetricsFactory intentionally nil.
	}

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/no-factory", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/no-factory", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Nil(t, findDurationHistogram(t, reader),
		"nil MetricsFactory must not record duration")
}

// TestWithTelemetry_UnmatchedRouteOmitsHTTPRoute verifies the catch-all 404
// guard: Fiber's default unmatched-path handler exposes Route().Path=="/",
// which would conflate scanner/404 traffic with the actual root handler in
// dashboards. The middleware MUST omit http.route entirely from both the span
// and the metric in that case, while still recording http.route="/" for a
// legitimately-registered root handler.
func TestWithTelemetry_UnmatchedRouteOmitsHTTPRoute(t *testing.T) {
	t.Run("unmatched 404 omits http.route", func(t *testing.T) {
		tel, reader, spanExp := newTelemetryHarness(t)

		app := fiber.New()
		mid := NewTelemetryMiddleware(tel)
		app.Use(mid.WithTelemetry(tel))
		// No routes registered: any request hits Fiber's catch-all 404.

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/not-registered", nil))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)

		dp := findDurationHistogram(t, reader)
		require.NotNil(t, dp, "duration must still be recorded for the 404")
		assert.EqualValues(t, 1, dp.Count)

		_, hasRoute := dp.Attributes.Value(attribute.Key("http.route"))
		assert.False(t, hasRoute,
			"http.route must be absent on the metric for unmatched 404")

		spans := spanExp.GetSpans()
		require.NotEmpty(t, spans)
		assert.Empty(t, getSpanAttr(spans[0], "http.route"),
			"http.route must be absent on the span for unmatched 404")
	})

	t.Run("registered root handler retains http.route", func(t *testing.T) {
		tel, reader, spanExp := newTelemetryHarness(t)

		app := fiber.New()
		mid := NewTelemetryMiddleware(tel)
		app.Use(mid.WithTelemetry(tel))
		app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		dp := findDurationHistogram(t, reader)
		require.NotNil(t, dp)
		route, ok := attrValue(dp.Attributes, "http.route")
		require.True(t, ok,
			"http.route must be present for a legitimately-registered root handler")
		assert.Equal(t, "/", route)

		spans := spanExp.GetSpans()
		require.NotEmpty(t, spans)
		assert.Equal(t, "/", getSpanAttr(spans[0], "http.route"))
	})
}

func TestWithTelemetry_EarlyBodyLimitMiddlewareRefusalRecordsStatusWithoutRoute(t *testing.T) {
	t.Parallel()

	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))
	app.Use(func(c fiber.Ctx) error {
		if c.Request().Header.ContentLength() > 16 {
			return fiber.ErrRequestEntityTooLarge
		}

		return c.Next()
	})
	app.Post("/", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", 32)))
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp, "duration must be recorded for early refusals")
	assert.EqualValues(t, 1, dp.Count)

	status, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusRequestEntityTooLarge, status.AsInt64())

	_, hasMetricRoute := dp.Attributes.Value(attribute.Key("http.route"))
	assert.False(t, hasMetricRoute, "an early refusal has no matched route")

	spans := spanExp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "413", getSpanAttr(spans[0], "http.response.status_code"))
	assert.Empty(t, getSpanAttr(spans[0], "http.route"))
}

// TestWithTelemetry_NormalizesUnknownMethodOnSpan verifies that an unknown
// HTTP verb is normalized to "_OTHER" on both the metric and the span, with
// the original verb preserved exclusively on the span's
// http.request.method_original attribute (never on the metric, to keep
// label cardinality bounded).
func TestWithTelemetry_NormalizesUnknownMethodOnSpan(t *testing.T) {
	tel, reader, spanExp := newTelemetryHarness(t)

	// Fiber rejects unknown verbs at the framework boundary by default.
	// Extend RequestMethods so the middleware actually sees PROPFIND.
	app := fiber.New(fiber.Config{
		RequestMethods: append(fiber.DefaultMethods, "PROPFIND"),
	})
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Add([]string{"PROPFIND"}, "/dav/:resource", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest("PROPFIND", "/dav/foo", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Metric assertions: normalized to "_OTHER", no method_original.
	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	method, ok := attrValue(dp.Attributes, "http.request.method")
	require.True(t, ok)
	assert.Equal(t, "_OTHER", method)

	_, hasOrigOnMetric := dp.Attributes.Value(attribute.Key("http.request.method_original"))
	assert.False(t, hasOrigOnMetric, "method_original must NEVER appear on the metric")

	// Span assertions: normalized method + original preserved.
	spans := spanExp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "_OTHER", getSpanAttr(spans[0], "http.request.method"))
	assert.Equal(t, "PROPFIND", getSpanAttr(spans[0], "http.request.method_original"))
}

// TestWithTelemetry_KnownMethodHasNoOriginal verifies that a canonical method
// (GET) does not emit http.request.method_original anywhere.
func TestWithTelemetry_KnownMethodHasNoOriginal(t *testing.T) {
	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/ping", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ping", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)
	method, _ := attrValue(dp.Attributes, "http.request.method")
	assert.Equal(t, "GET", method)
	_, hasOrig := dp.Attributes.Value(attribute.Key("http.request.method_original"))
	assert.False(t, hasOrig)

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)
	assert.Equal(t, "GET", getSpanAttr(spans[0], "http.request.method"))
	assert.Empty(t, getSpanAttr(spans[0], "http.request.method_original"))
}

// TestWithTelemetry_TruncatesLongUserAgent verifies the user_agent.original
// span attribute is capped at maxUserAgentAttrLen bytes regardless of input
// length, protecting trace storage from pathological clients.
func TestWithTelemetry_TruncatesLongUserAgent(t *testing.T) {
	tel, _, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	longUA := strings.Repeat("a", 4000)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("User-Agent", longUA)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)

	ua := getSpanAttr(spans[0], "user_agent.original")
	assert.Len(t, ua, maxUserAgentAttrLen)
	assert.Equal(t, strings.Repeat("a", maxUserAgentAttrLen), ua)
}

// TestWithTelemetry_TruncatesUserAgentAtRuneBoundary verifies that a multi-byte
// UTF-8 user-agent is truncated at a rune boundary, never mid-codepoint, so
// the resulting span attribute is always valid UTF-8 and never exceeds the
// byte cap. Uses a 3-byte rune ("€" = 0xE2 0x82 0xAC) repeated so the byte
// cap (256) falls strictly inside a codepoint; a naive byte slice would
// produce invalid UTF-8 at the boundary.
func TestWithTelemetry_TruncatesUserAgentAtRuneBoundary(t *testing.T) {
	tel, _, spanExp := newTelemetryHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	longUA := strings.Repeat("€", 1000) // 3000 bytes
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("User-Agent", longUA)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)

	ua := getSpanAttr(spans[0], "user_agent.original")
	assert.LessOrEqual(t, len(ua), maxUserAgentAttrLen,
		"truncated user-agent must not exceed the byte cap")
	assert.True(t, utf8.ValidString(ua),
		"truncated user-agent must remain valid UTF-8 (never split a codepoint)")
	// 256 / 3 = 85 complete "€" runes (255 bytes), which is the largest
	// rune-aligned prefix that fits within the cap.
	assert.Equal(t, strings.Repeat("€", 85), ua)
}

// TestWithTelemetry_DurationOmitsTenantIDFromHeader verifies that
// client-controlled identity never becomes an HTTP metric label.
func TestWithTelemetry_DurationOmitsTenantIDFromHeader(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/orders", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	req.Header.Set("X-Tenant-Id", "acme")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)
	assert.EqualValues(t, 1, dp.Count)

	_, ok := attrValue(dp.Attributes, "tenant.id")
	assert.False(t, ok, "tenant.id must never label HTTP request duration")
}

// TestWithTelemetry_DurationOmitsTenantIDFromBaggage verifies that identity is
// excluded even when it was propagated by a trusted upstream service. HTTP RED
// metrics retain transport dimensions only.
func TestWithTelemetry_DurationOmitsTenantIDFromBaggage(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)

	// Seed the request UserContext with baggage-propagated tenant.id before the
	// telemetry middleware runs, mirroring an upstream service that injected
	// tenant.id into the OTel baggage rather than the X-Tenant-Id header.
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(ctxWithBaggageTenant(t, "acme"))
		return c.Next()
	})
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/orders", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	// No X-Tenant-Id header: the only tenant source is the inbound baggage.
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/orders", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)
	assert.EqualValues(t, 1, dp.Count)

	_, ok := attrValue(dp.Attributes, "tenant.id")
	assert.False(t, ok, "tenant.id must never label HTTP request duration")
}

// TestWithTelemetry_RecordsDurationWithoutTenantID verifies that the
// tenant.id attribute is omitted entirely when the request does not carry the
// canonical header. Series for non-tenant traffic (probes, internal callers)
// must not gain an empty label that would split the time series.
func TestWithTelemetry_RecordsDurationWithoutTenantID(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/orders", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/orders", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	_, ok := attrValue(dp.Attributes, "tenant.id")
	assert.False(t, ok, "tenant.id must be absent when no X-Tenant-Id header was supplied")
}

// TestWithTelemetry_DurationOmitsOversizedTenantID verifies that even a value
// outside the compatibility resolver's cap cannot become an HTTP metric label.
func TestWithTelemetry_DurationOmitsOversizedTenantID(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/orders", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	req.Header.Set("X-Tenant-Id", strings.Repeat("a", constant.MaxTenantIDLen+1))

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	_, ok := attrValue(dp.Attributes, "tenant.id")
	assert.False(t, ok, "tenant.id must be absent when the header value exceeds the cap")
}
