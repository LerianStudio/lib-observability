//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LerianStudio/lib-observability/metrics"
	"github.com/LerianStudio/lib-observability/tracing"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

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
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
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

// TestWithTelemetry_RecordsDurationOnSuccess verifies that a successful 200
// request emits the duration histogram with the route template (not the raw
// path) and no error.type attribute.
func TestWithTelemetry_RecordsDurationOnSuccess(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/users/:id", func(c *fiber.Ctx) error {
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

	app.Get("/api/items/:id", func(c *fiber.Ctx) error {
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

	app.Get("/api/health", func(c *fiber.Ctx) error {
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
}

// TestWithTelemetry_RecordsDurationOnHandlerError verifies that a handler-
// returned error sets error.type to the Go type name (rather than a status
// classification), preserving the originating error identity.
func TestWithTelemetry_RecordsDurationOnHandlerError(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return c.Status(http.StatusInternalServerError).SendString(err.Error())
		},
	})
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	sentinel := errors.New("boom")
	app.Get("/api/explode", func(c *fiber.Ctx) error {
		return sentinel
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/explode", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp)

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok, "handler error must set error.type")
	// errors.errorString is the concrete type used by errors.New.
	assert.Equal(t, "errors.errorString", errType,
		"handler error type must surface the originating Go type name")
}

// TestWithTelemetry_RecordsDurationOnFiberError4xx verifies that a handler
// returning fiber.NewError(4xx) records the effective HTTP status from the
// error (not the unwritten default response status), and surfaces the
// originating *fiber.Error type as error.type. The duration metric must
// reflect what an end-user actually observes after Fiber's error handler runs.
func TestWithTelemetry_RecordsDurationOnFiberError4xx(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/items/:id", func(c *fiber.Ctx) error {
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

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok, "handler-returned *fiber.Error must set error.type")
	assert.Equal(t, "fiber.Error", errType)
}

// TestWithTelemetry_RecordsDurationOnFiberError400 verifies the same contract
// for 4xx bad-request errors raised via fiber.NewError, asserted independently
// from the 404 case to catch regressions for either code path.
func TestWithTelemetry_RecordsDurationOnFiberError400(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Post("/api/validate", func(c *fiber.Ctx) error {
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

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok)
	assert.Equal(t, "fiber.Error", errType)
}

// TestWithTelemetry_RecordsDurationOnFiberError5xx verifies that a 5xx
// fiber.NewError is also reflected in status_code on the duration metric.
func TestWithTelemetry_RecordsDurationOnFiberError5xx(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/down", func(c *fiber.Ctx) error {
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
	assert.Equal(t, "fiber.Error", errType)
}

// TestWithTelemetry_GenericHandlerErrorStatusCodeIs500 verifies that when a
// handler returns a non-fiber error and no custom ErrorHandler has rewritten
// the status code by the time the metric is recorded, the metric still
// reports 500, matching what Fiber's default error handler will write to
// the client.
func TestWithTelemetry_GenericHandlerErrorStatusCodeIs500(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	// Use Fiber's default ErrorHandler (no override) so the response status
	// is materialized AFTER the WithTelemetry middleware unwinds.
	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/explode", func(c *fiber.Ctx) error {
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
	assert.Equal(t, "errors.errorString", errType)
}

// TestWithTelemetry_DoesNotRecordForExcludedRoute verifies that excluded
// routes bypass duration recording entirely.
func TestWithTelemetry_DoesNotRecordForExcludedRoute(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel, "/swagger"))

	app.Get("/swagger/index.html", func(c *fiber.Ctx) error {
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

	app.Get("/ping", func(c *fiber.Ctx) error {
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

	app.Get("/no-mp", func(c *fiber.Ctx) error {
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

	app.Get("/no-factory", func(c *fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/no-factory", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Nil(t, findDurationHistogram(t, reader),
		"nil MetricsFactory must not record duration")
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

	app.Add("PROPFIND", "/dav/:resource", func(c *fiber.Ctx) error {
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

	app.Get("/ping", func(c *fiber.Ctx) error {
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
