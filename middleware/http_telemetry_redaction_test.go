//go:build unit

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	obslog "github.com/LerianStudio/lib-observability/v2/log"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

func TestHTTPMiddleware_AdversarialRequest_EmitsOnlyRouteTemplate(t *testing.T) {
	// WithTelemetry installs process-wide telemetry state, so this test stays
	// sequential under the repository's global-state rule.
	const (
		routeTemplate         = "/v1/contratos/:numero_contrato"
		unmatchedRoute        = "/{unmatched}"
		dynamicContract       = "06881656483"
		afterCursor           = "opaque-after-cursor-value"
		throughCursor         = "opaque-through-cursor-value"
		unauthenticatedTenant = "customer-identity-from-header"
	)

	tests := []struct {
		name            string
		requestTarget   string
		registerRoute   bool
		wantStatus      int
		wantRoute       string
		wantMetricRoute bool
	}{
		{
			name:            "matched dynamic route",
			requestTarget:   "/v1/contratos/" + dynamicContract + "?after=" + afterCursor + "&through=" + throughCursor,
			registerRoute:   true,
			wantStatus:      http.StatusOK,
			wantRoute:       routeTemplate,
			wantMetricRoute: true,
		},
		{
			name:            "unmatched route",
			requestTarget:   "/scanner/" + dynamicContract + "?after=" + afterCursor + "&through=" + throughCursor,
			registerRoute:   false,
			wantStatus:      http.StatusNotFound,
			wantRoute:       unmatchedRoute,
			wantMetricRoute: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			tel, reader, spanExporter := newTelemetryHarness(t)
			logger := &captureLogger{}

			app := fiber.New()
			app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			if test.registerRoute {
				app.Get(routeTemplate, func(c fiber.Ctx) error {
					return c.SendStatus(http.StatusOK)
				})
			}

			req := httptest.NewRequest(http.MethodGet, test.requestTarget, nil)
			req.Header.Set("X-Tenant-Id", unauthenticatedTenant)

			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()
			assert.Equal(t, test.wantStatus, resp.StatusCode)

			messages, fields := logger.snapshot()
			require.Len(t, messages, 1)
			assert.Contains(t, messages[0], "GET "+test.wantRoute)
			assert.Contains(t, fields, obslog.String("http_path", test.wantRoute))
			assertLogHasNoForbiddenHTTPData(t, messages, fields,
				dynamicContract, afterCursor, throughCursor, unauthenticatedTenant, "after", "through")

			spans := spanExporter.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, "GET "+test.wantRoute, spans[0].Name)
			assert.Equal(t, test.wantRoute, getSpanAttr(spans[0], "url.path"))
			assertSpanHasNoForbiddenHTTPData(t, spans[0].Name, spans[0].Attributes,
				dynamicContract, afterCursor, throughCursor, unauthenticatedTenant, "after", "through")

			dataPoint := findDurationHistogram(t, reader)
			require.NotNil(t, dataPoint)
			metricRoute, hasMetricRoute := attrValue(dataPoint.Attributes, "http.route")
			assert.Equal(t, test.wantMetricRoute, hasMetricRoute)
			if test.wantMetricRoute {
				assert.Equal(t, routeTemplate, metricRoute)
			}
			assertMetricHasNoForbiddenHTTPData(t, dataPoint.Attributes,
				dynamicContract, afterCursor, throughCursor, unauthenticatedTenant, "after", "through")

			wantMetricKeys := []string{"http.request.method", "http.response.status_code"}
			if test.wantMetricRoute {
				wantMetricKeys = append(wantMetricKeys, "http.route")
			}
			assert.ElementsMatch(t, wantMetricKeys, metricAttributeKeys(dataPoint.Attributes))
		})
	}
}

func assertLogHasNoForbiddenHTTPData(
	t *testing.T,
	messages []string,
	fields []obslog.Field,
	forbidden ...string,
) {
	t.Helper()

	for _, value := range forbidden {
		for _, message := range messages {
			assert.NotContains(t, message, value)
		}
		for _, field := range fields {
			assert.NotContains(t, field.Key, "tenant")
			if stringValue, ok := field.Value.(string); ok {
				assert.NotContains(t, stringValue, value)
			}
		}
	}
}

func assertSpanHasNoForbiddenHTTPData(
	t *testing.T,
	spanName string,
	attrs []attribute.KeyValue,
	forbidden ...string,
) {
	t.Helper()

	for _, attr := range attrs {
		assert.NotContains(t, string(attr.Key), "tenant")
	}

	for _, value := range forbidden {
		assert.NotContains(t, spanName, value)
		for _, attr := range attrs {
			assert.NotContains(t, attr.Value.Emit(), value)
		}
	}
}

func assertMetricHasNoForbiddenHTTPData(
	t *testing.T,
	attrs attribute.Set,
	forbidden ...string,
) {
	t.Helper()

	for _, attr := range attrs.ToSlice() {
		assert.NotContains(t, string(attr.Key), "tenant")
		for _, value := range forbidden {
			assert.NotContains(t, attr.Value.Emit(), value)
		}
	}
}

func metricAttributeKeys(attrs attribute.Set) []string {
	keys := make([]string, 0, attrs.Len())
	for _, attr := range attrs.ToSlice() {
		keys = append(keys, string(attr.Key))
	}

	return keys
}
