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

	req, err := http.NewRequest(http.MethodGet, "/api/users/42", nil)
	require.NoError(t, err)

	resp, err := app.Test(req)
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
