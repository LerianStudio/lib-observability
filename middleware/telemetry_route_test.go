//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LerianStudio/lib-observability/tracing"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

func TestWithTelemetry_UnmatchedRouteDoesNotPanic(t *testing.T) {
	tp, spanRecorder := setupTestTracer(t)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTP)

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library", EnableTelemetry: true},
		TracerProvider:  tp,
	}

	app := fiber.New()
	app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))

	assert.NotPanics(t, func() {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/missing", nil))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	assert.NotEmpty(t, spanRecorder.Ended())
}
