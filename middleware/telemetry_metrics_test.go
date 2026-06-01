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

	app.Get("/api/raise", func(c *fiber.Ctx) error {
		return fiber.NewError(http.StatusServiceUnavailable, "down")
	})
	app.Get("/api/send", func(c *fiber.Ctx) error {
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
