//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// findActiveRequestsSum extracts the http.server.active_requests UpDownCounter
// value. Returns (value, true) if the metric exists, or (0, false) when absent.
// It also locks the unit to "{request}" per the metric contract.
func findActiveRequestsSum(
	t *testing.T,
	reader *sdkmetric.ManualReader,
) (int64, bool) {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != httpServerActiveRequestsMetric {
				continue
			}

			s, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected Int64 sum for %s, got %T", m.Name, m.Data)
			require.Equal(t, "{request}", m.Unit, "metric unit must be {request}")
			require.NotEmpty(t, s.DataPoints)

			return s.DataPoints[0].Value, true
		}
	}

	return 0, false
}

// TestWithTelemetry_ActiveRequestsSettlesToZero verifies that after a request
// completes, the active-requests UpDownCounter has been incremented and then
// decremented back to a net zero, and carries the http.request.method label.
func TestWithTelemetry_ActiveRequestsSettlesToZero(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	app.Get("/api/ping", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	value, present := findActiveRequestsSum(t, reader)
	require.True(t, present, "http.server.active_requests must be registered after a request")
	assert.EqualValues(t, 0, value,
		"active requests must settle back to zero after the request completes")
}

// TestWithTelemetry_ActiveRequestsIncrementsDuringHandler verifies the counter
// reads exactly 1 while a handler is mid-flight, proving increment happens
// before c.Next() and decrement after.
func TestWithTelemetry_ActiveRequestsIncrementsDuringHandler(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))

	var inFlight int64

	var methodDuringHandler string

	var hasMethodLabel bool

	app.Get("/api/ping", func(c fiber.Ctx) error {
		// Collect while still inside the handler: the counter must read 1.
		rm := &metricdata.ResourceMetrics{}
		require.NoError(t, reader.Collect(context.Background(), rm))

		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != httpServerActiveRequestsMetric {
					continue
				}

				s, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				require.NotEmpty(t, s.DataPoints)
				inFlight = s.DataPoints[0].Value

				mv, present := s.DataPoints[0].Attributes.Value(attribute.Key("http.request.method"))
				hasMethodLabel = present
				methodDuringHandler = mv.AsString()
			}
		}

		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.EqualValues(t, 1, inFlight,
		"active requests must read 1 while the handler is executing")
	require.True(t, hasMethodLabel, "active requests must carry http.request.method")
	assert.Equal(t, "GET", methodDuringHandler)
}

// TestWithTelemetry_ActiveRequestsNilMetricsFactoryDoesNotRecord verifies the
// active-requests counter is gated on MetricsFactory presence, matching the
// duration histogram.
func TestWithTelemetry_ActiveRequestsNilMetricsFactoryDoesNotRecord(t *testing.T) {
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

	app.Get("/api/ping", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	_, present := findActiveRequestsSum(t, reader)
	assert.False(t, present,
		"nil MetricsFactory must not register http.server.active_requests")
}
