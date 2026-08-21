//go:build unit

package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/metadata"
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

func findInt64SumByName(t *testing.T, reader *sdkmetric.ManualReader, name string) *metricdata.Sum[int64] {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			s, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected Int64 sum for %s, got %T", m.Name, m.Data)
			require.Equal(t, "{request}", m.Unit)

			return &s
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

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/users/42", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	counter := findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric)
	require.NotNil(t, counter)
	require.Len(t, counter.DataPoints, 1)
	counterDP := counter.DataPoints[0]
	assert.EqualValues(t, 1, counterDP.Value)
	assert.Equal(t, tenantID.String(), mustAttrValue(t, counterDP.Attributes, constant.AttrKeyTenantID))
	assert.Equal(t, "/api/users/:id", mustAttrValue(t, counterDP.Attributes, "http.route"))
	assertExactAttributeKeys(t, counterDP.Attributes, constant.AttrKeyTenantID, "http.route")

	responses5xx := findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric)
	require.NotNil(t, responses5xx)
	require.Len(t, responses5xx.DataPoints, 1)
	errorDP := responses5xx.DataPoints[0]
	assert.EqualValues(t, 1, errorDP.Value)
	assert.Equal(t, tenantID.String(), mustAttrValue(t, errorDP.Attributes, constant.AttrKeyTenantID))
	assert.Equal(t, "/api/users/:id", mustAttrValue(t, errorDP.Attributes, "http.route"))
	assertExactAttributeKeys(t, errorDP.Attributes, constant.AttrKeyTenantID, "http.route")
	assert.Nil(t, findInt64SumByName(t, reader, "lerian.http.server.errors.by_tenant"),
		"the unreleased legacy name must not be emitted alongside responses_5xx")
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric))

	latency := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric)
	require.NotNil(t, latency)
	require.Len(t, latency.DataPoints, 1)
	latencyDP := latency.DataPoints[0]
	assert.EqualValues(t, 1, latencyDP.Count)
	assert.Equal(t, tenantID.String(), mustAttrValue(t, latencyDP.Attributes, constant.AttrKeyTenantID))
	assert.Equal(t, "5xx", mustAttrValue(t, latencyDP.Attributes, "http.response.status_class"))
	assertExactAttributeKeys(t, latencyDP.Attributes,
		constant.AttrKeyTenantID, "http.response.status_class")
}

func TestAuthenticatedTenantHTTPMetrics_Responses5xxCounterOmitsOtherStatusClasses(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	tenantID := uuid.New()

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))
		return c.Next()
	})
	app.Get("/missing", func(c fiber.Ctx) error { return c.SendStatus(http.StatusNotFound) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/missing", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	requests := findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric)
	require.NotNil(t, requests)
	require.Len(t, requests.DataPoints, 1)
	assert.EqualValues(t, 1, requests.DataPoints[0].Value)
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric))

	responses4xx := findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric)
	require.NotNil(t, responses4xx)
	require.Len(t, responses4xx.DataPoints, 1)
	assert.EqualValues(t, 1, responses4xx.DataPoints[0].Value)
	assert.Equal(t, tenantID.String(),
		mustAttrValue(t, responses4xx.DataPoints[0].Attributes, constant.AttrKeyTenantID))
	assert.Equal(t, "/missing", mustAttrValue(t, responses4xx.DataPoints[0].Attributes, "http.route"))
	assertExactAttributeKeys(t, responses4xx.DataPoints[0].Attributes,
		constant.AttrKeyTenantID, "http.route")
}

func TestAuthenticatedTenantHTTPMetrics_Responses4xxCounterOmitsOtherStatusClasses(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	tenantID := uuid.New()

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))
		return c.Next()
	})
	app.Get("/unavailable", func(c fiber.Ctx) error { return c.SendStatus(http.StatusServiceUnavailable) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/unavailable", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric))
	responses5xx := findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric)
	require.NotNil(t, responses5xx)
	require.Len(t, responses5xx.DataPoints, 1)
	assert.EqualValues(t, 1, responses5xx.DataPoints[0].Value)
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

	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric))
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric))
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric))
	assert.Nil(t, findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric))
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
		ctx = baggage.ContextWithBaggage(ctx, bag)
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("tenant-id", "metadata-tenant"))
		c.SetContext(ctx)

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

	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric),
		"headers, baggage, gRPC metadata, and AttrBag attributes must not mint tenant series")
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric),
		"headers, baggage, gRPC metadata, and AttrBag attributes must not mint tenant series")
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric),
		"headers, baggage, gRPC metadata, and AttrBag attributes must not mint tenant series")
	assert.Nil(t, findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric),
		"headers, baggage, gRPC metadata, and AttrBag attributes must not mint tenant series")

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

	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric))
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric))
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric))
	assert.Nil(t, findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric))
}

func TestAuthenticatedTenantHTTPMetrics_DistinctTenantsProduceDistinctSeries(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	tenantIDs := []uuid.UUID{uuid.New(), uuid.New()}

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		tenantID, err := uuid.Parse(c.Get("X-Test-Authenticated-Tenant"))
		require.NoError(t, err)
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))

		return c.Next()
	})
	app.Get("/orders/:id", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	for _, tenantID := range tenantIDs {
		req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
		req.Header.Set("X-Test-Authenticated-Tenant", tenantID.String())
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	counter := findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric)
	require.NotNil(t, counter)
	require.Len(t, counter.DataPoints, 2)
	counterTenants := make(map[string]struct{}, 2)
	for _, dp := range counter.DataPoints {
		assert.EqualValues(t, 1, dp.Value)
		counterTenants[mustAttrValue(t, dp.Attributes, constant.AttrKeyTenantID)] = struct{}{}
	}
	assertTenantSet(t, counterTenants, tenantIDs)

	latency := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric)
	require.NotNil(t, latency)
	require.Len(t, latency.DataPoints, 2)
	latencyTenants := make(map[string]struct{}, 2)
	for _, dp := range latency.DataPoints {
		assert.EqualValues(t, 1, dp.Count)
		latencyTenants[mustAttrValue(t, dp.Attributes, constant.AttrKeyTenantID)] = struct{}{}
	}
	assertTenantSet(t, latencyTenants, tenantIDs)
}

func TestAuthenticatedTenantHTTPMetrics_ConcurrentRequestsDoNotMixTenants(t *testing.T) {
	const requestCount = 16

	tel, reader := newMetricsHarness(t)
	tenantIDs := make([]uuid.UUID, requestCount)
	for i := range tenantIDs {
		tenantIDs[i] = uuid.New()
	}

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		tenantID, err := uuid.Parse(c.Get("X-Test-Authenticated-Tenant"))
		if err != nil {
			return err
		}
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))

		return c.Next()
	})
	app.Get("/orders/:id", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	errCh := make(chan error, requestCount)
	var wg sync.WaitGroup
	for _, tenantID := range tenantIDs {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
			req.Header.Set("X-Test-Authenticated-Tenant", id.String())
			resp, err := app.Test(req)
			if err == nil {
				err = resp.Body.Close()
			}
			errCh <- err
		}(tenantID)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	counter := findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric)
	require.NotNil(t, counter)
	require.Len(t, counter.DataPoints, requestCount)
	counterTenants := make(map[string]struct{}, requestCount)
	var counterTotal int64
	for _, dp := range counter.DataPoints {
		counterTotal += dp.Value
		counterTenants[mustAttrValue(t, dp.Attributes, constant.AttrKeyTenantID)] = struct{}{}
	}
	assert.EqualValues(t, requestCount, counterTotal)
	assertTenantSet(t, counterTenants, tenantIDs)

	latency := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric)
	require.NotNil(t, latency)
	require.Len(t, latency.DataPoints, requestCount)
	latencyTenants := make(map[string]struct{}, requestCount)
	var latencyTotal uint64
	for _, dp := range latency.DataPoints {
		latencyTotal += dp.Count
		latencyTenants[mustAttrValue(t, dp.Attributes, constant.AttrKeyTenantID)] = struct{}{}
	}
	assert.EqualValues(t, requestCount, latencyTotal)
	assertTenantSet(t, latencyTenants, tenantIDs)
}

func TestAuthenticatedTenantHTTPMetrics_AuthenticatedTenantWinsOverForgedHeader(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	authenticatedTenant := uuid.New()
	forgedTenant := uuid.New()

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), authenticatedTenant))
		return c.Next()
	})
	app.Get("/health", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(constant.HeaderTenantID, forgedTenant.String())
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	counter := findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric)
	require.NotNil(t, counter)
	require.Len(t, counter.DataPoints, 1)
	assert.Equal(t, authenticatedTenant.String(),
		mustAttrValue(t, counter.DataPoints[0].Attributes, constant.AttrKeyTenantID))

	latency := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric)
	require.NotNil(t, latency)
	require.Len(t, latency.DataPoints, 1)
	assert.Equal(t, authenticatedTenant.String(),
		mustAttrValue(t, latency.DataPoints[0].Attributes, constant.AttrKeyTenantID))
}

func TestAuthenticatedTenantHTTPMetrics_StandardMetricStaysIdentityFreeAndSingleCounted(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), uuid.New()))
		return c.Next()
	})
	app.Get("/health", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/health", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	standard := findFloat64HistogramByName(t, reader, httpServerRequestDurationMetric)
	require.NotNil(t, standard)
	require.Len(t, standard.DataPoints, 1)
	assert.EqualValues(t, 1, standard.DataPoints[0].Count)
	_, hasTenant := standard.DataPoints[0].Attributes.Value(attribute.Key(constant.AttrKeyTenantID))
	assert.False(t, hasTenant, "standard HTTP metric must remain identity-free")
}

func TestAuthenticatedTenantLatency_OmitsRouteAndMethod(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), uuid.New()))
		return c.Next()
	})
	app.Get("/orders/:id", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/orders/42", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	latency := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric)
	require.NotNil(t, latency)
	require.Len(t, latency.DataPoints, 1)
	assertExactAttributeKeys(t, latency.DataPoints[0].Attributes,
		constant.AttrKeyTenantID, "http.response.status_class")
}

// TestAuthenticatedTenantLatency_AttributeSetIsFrozen pins the latency histogram
// attribute set. Each extra label multiplies series by explicit_boundary_count+3
// (17 today) and divides the tenant ceiling documented in
// docs/metrics-contract.md. http.route, http.request.method and the exact
// http.response.status_code are deliberately absent: per-route latency is a
// trace-level question. If this test fails, the documented budget no longer
// holds - recalculate it before changing the attribute set.
func TestAuthenticatedTenantLatency_AttributeSetIsFrozen(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	tenantID := uuid.New()

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))
		return c.Next()
	})
	app.Get("/orders/:id", func(c fiber.Ctx) error { return c.SendStatus(http.StatusNotFound) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/orders/42", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	latency := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric)
	require.NotNil(t, latency)
	require.Len(t, latency.DataPoints, 1)
	dataPoint := latency.DataPoints[0]
	require.Equal(t, 2, dataPoint.Attributes.Len())
	assert.Equal(t, tenantID.String(), mustAttrValue(t, dataPoint.Attributes, constant.AttrKeyTenantID))
	assert.Equal(t, "4xx", mustAttrValue(t, dataPoint.Attributes, "http.response.status_class"))
	assertExactAttributeKeys(t, dataPoint.Attributes,
		constant.AttrKeyTenantID, "http.response.status_class")
}

// TestAuthenticatedTenantCounters_AttributeSetIsFrozen pins the per-tenant counter
// attribute set to tenant.id x http.route. Each extra label divides the tenant
// ceiling - floor((cardinality limit - 1) / normalized routes) - by that label's
// cardinality. Adding http.response.status_code here would have produced 9000
// attribute sets for 50 tenants x 30 routes against a default limit of 2000,
// silently dropping tenants from per-tenant filtering. Recalculate the budget in
// docs/metrics-contract.md before changing this.
func TestAuthenticatedTenantCounters_AttributeSetIsFrozen(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	tenantID := uuid.New()

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))
		return c.Next()
	})
	app.Get("/orders/:outcome", func(c fiber.Ctx) error {
		switch c.Params("outcome") {
		case "missing":
			return c.SendStatus(http.StatusNotFound)
		case "unavailable":
			return c.SendStatus(http.StatusServiceUnavailable)
		default:
			return c.SendStatus(http.StatusOK)
		}
	})

	outcomes := []string{
		"ok", "ok", "ok", "ok", "ok",
		"missing", "missing", "missing",
		"unavailable", "unavailable",
	}
	for _, outcome := range outcomes {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/orders/"+outcome, nil))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	tests := []struct {
		name       string
		metricName string
		wantValue  int64
	}{
		{name: "requests", metricName: authenticatedTenantHTTPServerRequestsMetric, wantValue: 10},
		{name: "responses 4xx", metricName: authenticatedTenantHTTPServerResponses4xxMetric, wantValue: 3},
		{name: "responses 5xx", metricName: authenticatedTenantHTTPServerResponses5xxMetric, wantValue: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := findInt64SumByName(t, reader, tt.metricName)
			require.NotNil(t, counter)
			require.Len(t, counter.DataPoints, 1)
			dataPoint := counter.DataPoints[0]
			assert.Equal(t, tt.wantValue, dataPoint.Value)
			require.Equal(t, 2, dataPoint.Attributes.Len())
			assert.Equal(t, tenantID.String(),
				mustAttrValue(t, dataPoint.Attributes, constant.AttrKeyTenantID))
			assert.Equal(t, "/orders/:outcome", mustAttrValue(t, dataPoint.Attributes, "http.route"))
			assertExactAttributeKeys(t, dataPoint.Attributes, constant.AttrKeyTenantID, "http.route")
		})
	}
}

func TestAuthenticatedTenantCounters_UnmatchedRouteUsesBoundedFallback(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	tenantID := uuid.New()

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))
		return c.Next()
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/does-not-exist/42", nil))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	for _, metricName := range []string{
		authenticatedTenantHTTPServerRequestsMetric,
		authenticatedTenantHTTPServerResponses4xxMetric,
	} {
		counter := findInt64SumByName(t, reader, metricName)
		require.NotNil(t, counter)
		require.Len(t, counter.DataPoints, 1)
		dataPoint := counter.DataPoints[0]
		assert.Equal(t, int64(1), dataPoint.Value)
		assert.Equal(t, tenantID.String(),
			mustAttrValue(t, dataPoint.Attributes, constant.AttrKeyTenantID))
		assert.Equal(t, unmatchedRouteTemplate, mustAttrValue(t, dataPoint.Attributes, "http.route"))
		assertExactAttributeKeys(t, dataPoint.Attributes, constant.AttrKeyTenantID, "http.route")
	}
}

func TestAuthenticatedTenantHTTPMetrics_GRPCMetadataDoesNotMintTenant(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	app := fiber.New()
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Use(func(c fiber.Ctx) error {
		ctx := metadata.NewIncomingContext(c.Context(), metadata.Pairs("tenant-id", uuid.NewString()))
		c.SetContext(ctx)
		return c.Next()
	})
	app.Get("/health", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/health", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric))
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric))
	assert.Nil(t, findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric))
	assert.Nil(t, findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric))
}

func TestAuthenticatedTenantHTTPMetrics_AuthenticationMayRunBeforeTelemetry(t *testing.T) {
	tel, reader := newMetricsHarness(t)
	tenantID := uuid.New()

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithAuthenticatedTenantID(c.Context(), tenantID))
		return c.Next()
	})
	mid := NewTelemetryMiddleware(tel)
	app.Use(mid.WithAuthenticatedTenantHTTPMetrics(tel))
	app.Get("/health", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/health", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	requests := findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric)
	require.NotNil(t, requests)
	require.Len(t, requests.DataPoints, 1)
	assert.Equal(t, tenantID.String(),
		mustAttrValue(t, requests.DataPoints[0].Attributes, constant.AttrKeyTenantID))
}

func TestIsHTTPServerErrorBoundsStatusClass(t *testing.T) {
	t.Parallel()

	assert.False(t, isHTTPServerError(499))
	assert.True(t, isHTTPServerError(500))
	assert.True(t, isHTTPServerError(599))
	assert.False(t, isHTTPServerError(600))
	assert.False(t, isHTTPServerError(9999))
}

func TestIsHTTPClientErrorBoundsStatusClass(t *testing.T) {
	t.Parallel()

	assert.False(t, isHTTPClientError(399))
	assert.True(t, isHTTPClientError(400))
	assert.True(t, isHTTPClientError(499))
	assert.False(t, isHTTPClientError(500))
	assert.False(t, isHTTPClientError(9999))
}

func TestClassifyHTTPStatusClassBoundsArbitraryValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		want   string
	}{
		{-1, "other"},
		{99, "other"},
		{100, "1xx"},
		{199, "1xx"},
		{200, "2xx"},
		{399, "3xx"},
		{499, "4xx"},
		{599, "5xx"},
		{600, "other"},
		{9999, "other"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, classifyHTTPStatusClass(tt.status))
	}
}

func TestAuthenticatedTenantHTTPMetrics_DocumentedScenarioDoesNotOverflow(t *testing.T) {
	const (
		tenantCount = 50
		routeCount  = 30
	)
	statusClasses := []string{"1xx", "2xx", "3xx", "4xx", "5xx", "other"}

	tel, reader := newMetricsHarness(t)
	instruments := newHTTPServerInstruments(tel, true)
	require.NotNil(t, instruments.tenantRequests)
	require.NotNil(t, instruments.tenant5xx)
	require.NotNil(t, instruments.tenant4xx)
	require.NotNil(t, instruments.tenantLatency)

	ctx := context.Background()
	for tenantIndex := 0; tenantIndex < tenantCount; tenantIndex++ {
		tenantAttr := attribute.String(constant.AttrKeyTenantID, uuid.NewString())
		for routeIndex := 0; routeIndex < routeCount; routeIndex++ {
			routeAttr := attribute.String("http.route", fmt.Sprintf("/route/%d", routeIndex))
			attrs := metric.WithAttributes(tenantAttr, routeAttr)
			instruments.tenantRequests.Add(ctx, 1, attrs)
			instruments.tenant5xx.Add(ctx, 1, attrs)
			instruments.tenant4xx.Add(ctx, 1, attrs)
		}
		for _, statusClass := range statusClasses {
			instruments.tenantLatency.Record(ctx, 0.1, metric.WithAttributes(
				tenantAttr,
				attribute.String("http.response.status_class", statusClass),
			))
		}
	}

	requests := findInt64SumByName(t, reader, authenticatedTenantHTTPServerRequestsMetric)
	require.NotNil(t, requests)
	require.Len(t, requests.DataPoints, tenantCount*routeCount)
	assertNoOverflowInt64(t, requests.DataPoints)

	responses5xx := findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses5xxMetric)
	require.NotNil(t, responses5xx)
	require.Len(t, responses5xx.DataPoints, tenantCount*routeCount)
	assertNoOverflowInt64(t, responses5xx.DataPoints)

	responses4xx := findInt64SumByName(t, reader, authenticatedTenantHTTPServerResponses4xxMetric)
	require.NotNil(t, responses4xx)
	require.Len(t, responses4xx.DataPoints, tenantCount*routeCount)
	assertNoOverflowInt64(t, responses4xx.DataPoints)

	latency := findFloat64HistogramByName(t, reader, authenticatedTenantHTTPServerLatencyMetric)
	require.NotNil(t, latency)
	require.Len(t, latency.DataPoints, tenantCount*len(statusClasses))
	for _, dp := range latency.DataPoints {
		_, overflow := dp.Attributes.Value(attribute.Key("otel.metric.overflow"))
		assert.False(t, overflow)
	}
}

func assertNoOverflowInt64(t *testing.T, dataPoints []metricdata.DataPoint[int64]) {
	t.Helper()
	for _, dp := range dataPoints {
		_, overflow := dp.Attributes.Value(attribute.Key("otel.metric.overflow"))
		assert.False(t, overflow)
	}
}

func mustAttrValue(t *testing.T, attrs attribute.Set, key string) string {
	t.Helper()

	value, ok := attrValue(attrs, key)
	require.True(t, ok, "expected attribute %s", key)

	return value
}

func assertExactAttributeKeys(t *testing.T, attrs attribute.Set, expected ...string) {
	t.Helper()

	actual := make([]string, 0, attrs.Len())
	for iter := attrs.Iter(); iter.Next(); {
		actual = append(actual, string(iter.Attribute().Key))
	}
	assert.ElementsMatch(t, expected, actual)
}

func assertTenantSet(t *testing.T, actual map[string]struct{}, expected []uuid.UUID) {
	t.Helper()

	require.Len(t, actual, len(expected))
	for _, tenantID := range expected {
		_, ok := actual[tenantID.String()]
		assert.True(t, ok, "missing tenant series %s", tenantID)
	}
}
