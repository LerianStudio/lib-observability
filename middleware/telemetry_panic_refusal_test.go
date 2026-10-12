//go:build unit

package middleware

import (
	"context"
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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Fiber runs the Use chain before rendering a pre-routing 413: no span for that
// pass, and the duration metric counts the 413 itself, without a route.
func TestWithTelemetryCountsRequestRefusedBeforeRoutingWithoutSpan(t *testing.T) {
	t.Parallel()

	tel, reader, spanExp := newTelemetryHarness(t)

	app := fiber.New(fiber.Config{BodyLimit: 16})
	LogRefusalsBeforeRouting(app, WithCustomLogger(&captureLogger{}))
	app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
	app.Post("/contracts", func(c fiber.Ctx) error { return c.SendString("ok") })

	_, err := app.Test(httptest.NewRequest(http.MethodPost, "/contracts", strings.NewReader(strings.Repeat("a", 64))))
	require.ErrorIs(t, err, fasthttp.ErrBodyTooLarge)

	assert.Empty(t, spanExp.GetSpans())

	dp := findDurationHistogram(t, reader)
	require.NotNil(t, dp, "the refusal must be counted")
	assert.EqualValues(t, 1, dp.Count)

	status, ok := dp.Attributes.Value(attribute.Key("http.response.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, http.StatusRequestEntityTooLarge, status.AsInt64())

	method, ok := dp.Attributes.Value(attribute.Key("http.request.method"))
	require.True(t, ok)
	assert.Equal(t, http.MethodPost, method.AsString())

	_, hasRoute := dp.Attributes.Value(attribute.Key("http.route"))
	assert.False(t, hasRoute)
}

// Services mount recover OUTSIDE telemetry, so a panic unwinds through it.
func TestWithTelemetryEndsPanickingRequestAsServerErrorAndRepanics(t *testing.T) {
	// Starts the process-wide metrics collector: stays sequential.
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

// A span EndTracingSpans ends without owning it (here the handler's) must be
// exported, failed, when the handler panics.
func TestEndTracingSpansEndsPanickingRequestSpanAsFailedAndRepanics(t *testing.T) {
	t.Parallel()

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	var outerRecovered any

	app := fiber.New()
	app.Use(recover.New(recover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: func(_ fiber.Ctx, recovered any) { outerRecovered = recovered },
	}))
	app.Use(NewTelemetryMiddleware(nil).EndTracingSpans)
	app.Get("/boom", func(c fiber.Ctx) error {
		ctx, _ := tp.Tracer("test").Start(c.Context(), "handler-span")
		c.SetContext(ctx)

		panic("boom")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "boom", outerRecovered, "the panic must still reach the outer recover")

	spans := spanExp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "handler-span", spans[0].Name)
	assert.Equal(t, codes.Error, spans[0].Status.Code)
	assert.Equal(t, "panic: boom", spans[0].Status.Description)
}
