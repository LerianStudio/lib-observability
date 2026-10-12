//go:build unit

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Fiber runs the Use chain before rendering a pre-routing 413; telemetry must
// not record that pass as a 200 on /{unmatched}.
func TestWithTelemetrySkipsRequestRefusedBeforeRouting(t *testing.T) {
	t.Parallel()

	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New(fiber.Config{BodyLimit: 16})
	LogRefusalsBeforeRouting(app, WithCustomLogger(&captureLogger{}))
	app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
	app.Post("/contracts", func(c fiber.Ctx) error { return c.SendString("ok") })

	_, err := app.Test(httptest.NewRequest(http.MethodPost, "/contracts", strings.NewReader(strings.Repeat("a", 64))))
	require.ErrorIs(t, err, fasthttp.ErrBodyTooLarge)

	assert.Empty(t, spanExp.GetSpans())
	assert.Nil(t, findDurationHistogram(t, reader))
}

// Services mount recover OUTSIDE telemetry, so a panic unwinds through it.
func TestWithTelemetryEndsPanickingRequestAsServerErrorAndRepanics(t *testing.T) {
	t.Parallel()

	tel, reader, spanExp := newTelemetryHarness(t)

	var outerRecovered any

	app := fiber.New()
	app.Use(recover.New(recover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: func(_ fiber.Ctx, recovered any) { outerRecovered = recovered },
	}))
	app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
	app.Get("/boom", func(fiber.Ctx) error { panic("boom") })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "boom", outerRecovered, "the panic must still reach the outer recover")

	spans := spanExp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /boom", spans[0].Name)
	assert.Equal(t, codes.Error, spans[0].Status.Code)

	msg, found := findEventAttr(spans, "exception", "exception.message")
	require.True(t, found)
	assert.Equal(t, "panic: boom", msg)

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp, "the panicking request must be counted")
	status, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusInternalServerError, status.AsInt64())
}
