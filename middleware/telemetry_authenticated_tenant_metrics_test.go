//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	observability "github.com/LerianStudio/lib-observability/v3"
	constant "github.com/LerianStudio/lib-observability/v3/constants"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var _ func(*tracing.Telemetry) *TelemetryMiddleware = NewTelemetryMiddleware

func findFloat64HistogramByName(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	name string,
) *metricdata.Histogram[float64] {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected Float64 histogram for %s, got %T", m.Name, m.Data)
			require.Equal(t, "s", m.Unit)

			return &h
		}
	}

	return nil
}

func TestAuthenticatedTenantHTTPMetrics_RecordsExplicitlyAttestedIdentity(t *testing.T) {
	tel, reader, _ := newTelemetryHarness(t)
	tenantID := uuid.New()

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))

		return c.Next()
	})
	app.Get("/api/users/:id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusServiceUnavailable)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	req.Header.Set(constant.HeaderTenantID, uuid.NewString())

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	hist := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerRequestDurationMetric)
	require.NotNil(t, hist)
	require.Len(t, hist.DataPoints, 1)

	dp := hist.DataPoints[0]
	assert.EqualValues(t, 1, dp.Count)
	assert.Equal(t, tenantID.String(), mustAttrValue(t, dp.Attributes, constant.AttrKeyTenantID))
	assert.Equal(t, http.MethodGet, mustAttrValue(t, dp.Attributes, "http.request.method"))
	assert.Equal(t, "/api/users/:id", mustAttrValue(t, dp.Attributes, "http.route"))
	assert.Equal(t, "503", mustAttrValue(t, dp.Attributes, "error.type"))

	standard := findFloat64HistogramByName(t, reader, httpServerRequestDurationMetric)
	require.NotNil(t, standard)
	require.Len(t, standard.DataPoints, 1)
	_, hasTenant := standard.DataPoints[0].Attributes.Value(attribute.Key(constant.AttrKeyTenantID))
	assert.False(t, hasTenant, "standard HTTP metric must remain identity-free")
}

func TestAuthenticatedTenantHTTPMetrics_IsDisabledByDefault(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithTelemetry(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), uuid.New()))

		return c.Next()
	})
	app.Get("/health", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/health", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Nil(t, findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerRequestDurationMetric))
	assert.NotNil(t, findFloat64HistogramByName(t, reader, httpServerRequestDurationMetric))
}

func TestAuthenticatedTenantHTTPMetrics_IgnoresUntrustedIdentitySources(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	member, err := baggage.NewMember(constant.AttrKeyTenantID, "baggage-tenant")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		ctx := observability.ContextWithSpanAttributes(
			c.Context(),
			attribute.String(constant.AttrKeyTenantID, "attrbag-tenant"),
		)
		c.SetContext(baggage.ContextWithBaggage(ctx, bag))

		return c.Next()
	})
	app.Get("/api/users/:id", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	for i := 0; i < 25; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
		req.Header.Set(constant.HeaderTenantID, uuid.NewString())

		resp, requestErr := app.Test(req)
		require.NoError(t, requestErr)
		require.NoError(t, resp.Body.Close())
	}

	assert.Nil(t, findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerRequestDurationMetric),
		"headers, baggage, and generic span attributes must not mint tenant series")

	standard := findFloat64HistogramByName(t, reader, httpServerRequestDurationMetric)
	require.NotNil(t, standard)
	var count uint64
	for _, dp := range standard.DataPoints {
		count += dp.Count
	}
	assert.EqualValues(t, 25, count)
}

func TestAuthenticatedTenantHTTPMetrics_OmitsNilTenant(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), uuid.Nil))

		return c.Next()
	})
	app.Get("/health", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/health", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Nil(t, findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerRequestDurationMetric))
}

func mustAttrValue(t *testing.T, attrs attribute.Set, key string) string {
	t.Helper()

	value, ok := attrValue(attrs, key)
	require.True(t, ok, "expected attribute %s", key)

	return value
}
