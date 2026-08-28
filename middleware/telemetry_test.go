//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LerianStudio/lib-observability/v4/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// setupTestTracer sets up a test tracer provider and returns it along with a span recorder.
func setupTestTracer(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spanRecorder),
	)

	oldPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	t.Cleanup(func() {
		otel.SetTextMapPropagator(oldPropagator)
	})

	return tracerProvider, spanRecorder
}

// TestWithTelemetry tests the WithTelemetry middleware function.
func TestWithTelemetry(t *testing.T) {
	tests := []struct {
		name               string
		path               string
		route              string
		method             string
		setupHandler       func(c fiber.Ctx) error
		nilTelemetry       bool
		traceparent        string
		expectedStatusCode int
		expectSpan         bool
		swaggerPath        bool
	}{
		{
			name:               "Basic middleware functionality",
			path:               "/api/resource",
			method:             "GET",
			setupHandler:       func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) },
			expectedStatusCode: http.StatusOK,
			expectSpan:         true,
		},
		{
			name:               "Handler returns error",
			path:               "/api/resource",
			method:             "POST",
			setupHandler:       func(c fiber.Ctx) error { return errors.New("handler error") },
			expectedStatusCode: http.StatusInternalServerError,
			expectSpan:         true,
		},
		{
			name:               "Nil telemetry",
			path:               "/api/resource",
			method:             "GET",
			setupHandler:       func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) },
			nilTelemetry:       true,
			expectedStatusCode: http.StatusOK,
			expectSpan:         false,
		},
		{
			name:               "With trace context",
			path:               "/api/resource",
			method:             "GET",
			setupHandler:       func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) },
			traceparent:        "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			expectedStatusCode: http.StatusOK,
			expectSpan:         true,
		},
		{
			name:               "UUID in path",
			path:               "/api/users/123e4567-e89b-12d3-a456-426614174000/profile",
			route:              "/api/users/:id/profile",
			method:             "GET",
			setupHandler:       func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) },
			expectedStatusCode: http.StatusOK,
			expectSpan:         true,
		},
		{
			name:               "Swagger path bypass",
			path:               "/swagger/api-docs",
			method:             "GET",
			setupHandler:       func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) },
			expectedStatusCode: http.StatusOK,
			expectSpan:         false,
			swaggerPath:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			tp, spanRecorder := setupTestTracer(t)
			defer func() {
				_ = tp.Shutdown(ctx)
			}()

			oldTracerProvider := otel.GetTracerProvider()
			otel.SetTracerProvider(tp)
			defer otel.SetTracerProvider(oldTracerProvider)

			var tel *tracing.Telemetry
			if !tt.nilTelemetry {
				tel = &tracing.Telemetry{
					TelemetryConfig: tracing.TelemetryConfig{
						LibraryName:     "test-library",
						EnableTelemetry: true,
					},
					TracerProvider: tp,
				}
			}

			mid := NewTelemetryMiddleware(tel)

			app := fiber.New(fiber.Config{
				ErrorHandler: func(c fiber.Ctx, err error) error {
					return c.Status(http.StatusInternalServerError).SendString(err.Error())
				},
			})

			if !tt.nilTelemetry {
				if tt.swaggerPath {
					app.Use(mid.WithTelemetry(tel, "/swagger"))
				} else {
					app.Use(mid.WithTelemetry(tel))
				}
			}

			route := tt.path
			if tt.route != "" {
				route = tt.route
			}

			app.All(route, func(c fiber.Ctx) error {
				return tt.setupHandler(c)
			})

			req := httptest.NewRequest(tt.method, tt.path, nil)

			if tt.traceparent != "" {
				req.Header.Set("traceparent", tt.traceparent)
			}

			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, tt.expectedStatusCode, resp.StatusCode)

			spans := spanRecorder.Ended()

			if tt.expectSpan && !tt.nilTelemetry && !tt.swaggerPath {
				require.GreaterOrEqual(t, len(spans), 1, "Expected at least one span to be created")

				// The span is named "{method} {route template}": the template
				// registered with app.All (tt.route when set, else tt.path
				// registered literally). A :param route yields the template,
				// never the concrete value (also asserted in
				// TestWithTelemetry_SpanNameUsesRouteTemplate).
				expectedName := tt.method + " " + route

				spanFound := false
				for _, span := range spans {
					if span.Name() == expectedName {
						spanFound = true
						break
					}
				}
				assert.True(t, spanFound, "Expected span with name %s not found", expectedName)
			} else if tt.swaggerPath || tt.nilTelemetry {
				assert.Empty(t, spans, "Expected no spans for swagger path or nil telemetry")
			}
		})
	}
}

// stubTypedHandlerError is a distinctly-typed handler error so the exception
// event assertions below can prove the ORIGINAL type survives onto the span.
type stubTypedHandlerError struct{ msg string }

func (e *stubTypedHandlerError) Error() string { return e.msg }

// TestWithTelemetryRecordsOriginalExceptionType asserts the >=500 exception
// event carries the handler error's original Go type and sanitized message,
// not the type of an internal substitute error (errors.errorString).
func TestWithTelemetryRecordsOriginalExceptionType(t *testing.T) {
	ctx := context.Background()

	tp, spanRecorder := setupTestTracer(t)
	defer func() { _ = tp.Shutdown(ctx) }()

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{
			LibraryName:     "test-library",
			EnableTelemetry: true,
		},
		TracerProvider: tp,
	}

	mid := NewTelemetryMiddleware(tel)

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			return c.Status(http.StatusInternalServerError).SendString("boom")
		},
	})
	app.Use(mid.WithTelemetry(tel))
	app.Get("/api/resource", func(fiber.Ctx) error {
		return &stubTypedHandlerError{msg: "handler exploded"}
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/resource", nil))
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	var exceptionType, exceptionMessage string

	for _, span := range spanRecorder.Ended() {
		for _, event := range span.Events() {
			if event.Name != semconv.ExceptionEventName {
				continue
			}

			for _, attr := range event.Attributes {
				switch attr.Key {
				case semconv.ExceptionTypeKey:
					exceptionType = attr.Value.AsString()
				case semconv.ExceptionMessageKey:
					exceptionMessage = attr.Value.AsString()
				}
			}
		}
	}

	assert.Equal(t, "middleware.stubTypedHandlerError", exceptionType)
	assert.Equal(t, "handler exploded", exceptionMessage)
}

// TestWithTelemetryExcludedRoutes tests the WithTelemetry middleware with excluded routes.
func TestWithTelemetryExcludedRoutes(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		method         string
		excludedRoutes []string
		expectSpan     bool
	}{
		{
			name:           "Route not excluded",
			path:           "/api/users",
			method:         "GET",
			excludedRoutes: []string{"/swagger", "/health"},
			expectSpan:     true,
		},
		{
			name:           "Route excluded by exact match",
			path:           "/swagger/api-docs",
			method:         "GET",
			excludedRoutes: []string{"/swagger"},
			expectSpan:     false,
		},
		{
			name:           "Route excluded by partial match",
			path:           "/health/check",
			method:         "GET",
			excludedRoutes: []string{"/health"},
			expectSpan:     false,
		},
		{
			name:           "Multiple excluded routes",
			path:           "/metrics/prometheus",
			method:         "GET",
			excludedRoutes: []string{"/swagger", "/health", "/metrics"},
			expectSpan:     false,
		},
		{
			name:           "No excluded routes",
			path:           "/api/users",
			method:         "GET",
			excludedRoutes: []string{},
			expectSpan:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			tp, spanRecorder := setupTestTracer(t)
			defer func() {
				_ = tp.Shutdown(ctx)
			}()

			oldTracerProvider := otel.GetTracerProvider()
			otel.SetTracerProvider(tp)
			defer otel.SetTracerProvider(oldTracerProvider)

			tel := &tracing.Telemetry{
				TelemetryConfig: tracing.TelemetryConfig{
					LibraryName:     "test-library",
					EnableTelemetry: true,
				},
				TracerProvider: tp,
			}

			mid := NewTelemetryMiddleware(tel)

			app := fiber.New()
			app.Use(mid.WithTelemetry(tel, tt.excludedRoutes...))

			app.All(tt.path, func(c fiber.Ctx) error {
				return c.SendStatus(http.StatusOK)
			})

			req := httptest.NewRequest(tt.method, tt.path, nil)

			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			spans := spanRecorder.Ended()

			if tt.expectSpan {
				require.GreaterOrEqual(t, len(spans), 1, "Expected at least one span to be created")

				// Route registered literally via app.All(tt.path, ...), so the
				// matched template equals tt.path and the span is named
				// "{method} {template}".
				expectedSpanName := tt.method + " " + tt.path
				spanFound := false
				for _, span := range spans {
					if span.Name() == expectedSpanName {
						spanFound = true
						break
					}
				}
				assert.True(t, spanFound, "Expected span with name %s not found", expectedSpanName)
			} else {
				assert.Empty(t, spans, "Expected no spans for excluded routes")
			}
		})
	}
}

// TestEndTracingSpans tests the EndTracingSpans middleware function.
func TestEndTracingSpans(t *testing.T) {
	tests := []struct {
		name       string
		setupCtx   bool
		handlerErr error
	}{
		{
			name:       "With context",
			setupCtx:   true,
			handlerErr: nil,
		},
		{
			name:       "Without context",
			setupCtx:   false,
			handlerErr: nil,
		},
		{
			name:       "With context and handler error",
			setupCtx:   true,
			handlerErr: errors.New("handler error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spanRecorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithSpanProcessor(spanRecorder),
			)
			defer func() {
				_ = tp.Shutdown(context.Background())
			}()

			tel := &tracing.Telemetry{
				TelemetryConfig: tracing.TelemetryConfig{
					LibraryName:     "test-library",
					EnableTelemetry: true,
				},
				TracerProvider: tp,
			}

			mid := NewTelemetryMiddleware(tel)

			app := fiber.New()

			setupMiddleware := func(c fiber.Ctx) error {
				ctx := c.Context()
				if ctx == nil {
					ctx = context.Background()
				}

				if tt.setupCtx {
					tracer := tp.Tracer("test")
					ctx, _ = tracer.Start(ctx, "test-span")
					c.SetContext(ctx)
				}

				return c.Next()
			}

			handler := func(c fiber.Ctx) error {
				return tt.handlerErr
			}

			app.Get("/test", setupMiddleware, mid.EndTracingSpans, handler)

			req := httptest.NewRequest("GET", "/test", nil)
			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			if tt.handlerErr != nil {
				assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
			} else {
				assert.Equal(t, fiber.StatusOK, resp.StatusCode)
			}

			if tt.setupCtx {
				assert.Eventually(t, func() bool {
					return len(spanRecorder.Ended()) == 1
				}, time.Second, 10*time.Millisecond, "Expected middleware to end one span")

				spans := spanRecorder.Ended()
				if assert.Len(t, spans, 1) {
					assert.Equal(t, "test-span", spans[0].Name())
				}
			} else {
				assert.Never(t, func() bool {
					return len(spanRecorder.Ended()) > 0
				}, 100*time.Millisecond, 10*time.Millisecond, "Expected no spans to be ended")
			}
		})
	}
}

func TestEndTracingSpans_CallsNextWithoutInitialContext(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	mid := &TelemetryMiddleware{}
	handlerCalled := false

	app.Get("/test", mid.EndTracingSpans, func(c fiber.Ctx) error {
		handlerCalled = true
		return c.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.True(t, handlerCalled)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestEndTracingSpans_EndsFinalContextSpan(t *testing.T) {
	t.Parallel()

	tp, spanRecorder := setupTestTracer(t)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	app := fiber.New()
	mid := &TelemetryMiddleware{}

	app.Get("/test", mid.EndTracingSpans, func(c fiber.Ctx) error {
		ctx, _ := tp.Tracer("test").Start(context.Background(), "handler-span")
		c.SetContext(ctx)
		return c.SendStatus(http.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Eventually(t, func() bool {
		return len(spanRecorder.Ended()) == 1
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, "handler-span", spanRecorder.Ended()[0].Name())
}

// TestExtractHTTPContext tests the ExtractHTTPContext function from tracing package.
func TestExtractHTTPContext(t *testing.T) {
	ctx := context.Background()

	tp, _ := setupTestTracer(t)
	defer func() {
		_ = tp.Shutdown(ctx)
	}()

	oldTracerProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTracerProvider)

	traceparent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	app := fiber.New()

	app.Get("/test", func(c fiber.Ctx) error {
		ctx := ExtractHTTPContext(c.Context(), c)

		spanCtx := oteltrace.SpanContextFromContext(ctx)

		if c.Get("traceparent") != "" {
			assert.True(t, spanCtx.IsValid(), "Span context should be valid with traceparent header")
			assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", spanCtx.TraceID().String())
			assert.Equal(t, "00f067aa0ba902b7", spanCtx.SpanID().String())
		} else {
			assert.False(t, spanCtx.IsValid(), "Span context should not be valid without traceparent header")
		}

		return c.SendStatus(http.StatusOK)
	})

	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.Header.Set("traceparent", traceparent)

	resp1, err := app.Test(req1)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp1.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	req2 := httptest.NewRequest("GET", "/test", nil)

	resp2, err := app.Test(req2)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp2.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

// TestExtractHTTPContext_StripsTenantIDBaggage verifies that ExtractHTTPContext
// always removes an inbound tenant.id baggage member, unconditionally,
// regardless of any other configuration. An external client that sends
// `baggage: tenant.id=acme-corp` must not be able to forge the tenant
// identity stamped on this service's spans - tenant identity comes ONLY from
// a validated JWT claim. Other baggage members, and trace-context
// propagation itself, are left untouched.
func TestExtractHTTPContext_StripsTenantIDBaggage(t *testing.T) {
	// Not parallel: mutates the process-global OTel propagator, which
	// ExtractHTTPContext reads. The traceparent and baggage assertions below
	// require TraceContext and Baggage propagation to actually be installed,
	// not inherited from whichever test happened to run before.
	prevPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prevPropagator) })
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	app := fiber.New()

	var (
		gotTenant  string
		gotOther   string
		gotTraceID string
	)
	app.Get("/test", func(c fiber.Ctx) error {
		ctx := ExtractHTTPContext(c.Context(), c)
		gotTenant = baggage.FromContext(ctx).Member("tenant.id").Value()
		gotOther = baggage.FromContext(ctx).Member("region").Value()
		gotTraceID = oteltrace.SpanContextFromContext(ctx).TraceID().String()

		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("baggage", "tenant.id=acme-corp,region=us-east")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Empty(t, gotTenant, "tenant.id must never survive extraction from an inbound request")
	assert.Equal(t, "us-east", gotOther, "unrelated baggage members must pass through untouched")
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", gotTraceID,
		"trace-context propagation itself must be unaffected by the tenant.id strip")
}

// TestWithTelemetry_TrustInboundTraceContextDefaultsToFailClosed verifies
// FIX 6's default posture: with TrustInboundTraceContext left at its Go
// zero value (false), an inbound traceparent header is NOT honored - the
// service starts a fresh root span rather than letting an untrusted external
// caller choose this service's trace ID or force a sampling decision via a
// forged header. Setting the knob to true restores the previous
// (opt-in) behavior of joining the inbound trace.
func TestWithTelemetry_TrustInboundTraceContextDefaultsToFailClosed(t *testing.T) {
	t.Parallel()

	const injectedTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	traceparent := "00-" + injectedTraceID + "-00f067aa0ba902b7-01"

	tests := []struct {
		name       string
		trust      bool
		wantJoined bool
	}{
		{name: "default (unset) does not trust inbound trace context", trust: false},
		{name: "explicit opt-in trusts inbound trace context", trust: true, wantJoined: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tp, _ := setupTestTracer(t)
			defer func() { _ = tp.Shutdown(context.Background()) }()

			tel := &tracing.Telemetry{
				TelemetryConfig: tracing.TelemetryConfig{
					LibraryName:              "test-library",
					EnableTelemetry:          true,
					TrustInboundTraceContext: tt.trust,
				},
				TracerProvider: tp,
			}

			app := fiber.New()
			app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))

			var gotTraceID string
			app.Get("/test", func(c fiber.Ctx) error {
				gotTraceID = oteltrace.SpanContextFromContext(c.Context()).TraceID().String()

				return c.SendStatus(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("traceparent", traceparent)

			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()
			assert.Equal(t, http.StatusOK, resp.StatusCode)

			if tt.wantJoined {
				assert.Equal(t, injectedTraceID, gotTraceID, "trusted inbound trace context must be joined")
				return
			}

			assert.NotEqual(t, injectedTraceID, gotTraceID,
				"an untrusted inbound traceparent must never be honored by default")
		})
	}
}

// TestWithTelemetryTracePropagationIsIndependentOfUserAgent verifies that,
// for a service that has explicitly opted into trusting inbound trace
// context (TrustInboundTraceContext: true - e.g. one sitting behind a
// trusted ingress), valid W3C trace context is extracted for every caller,
// branded or not. The tenant.id baggage member must NEVER propagate from an
// inbound request, independent of that trust setting: tenant identity is a
// third rail that only comes from a validated JWT claim.
func TestWithTelemetryTracePropagationIsIndependentOfUserAgent(t *testing.T) {
	tests := []struct {
		name        string
		userAgent   string
		traceparent string
	}{
		{
			name:        "Internal Lerian service propagates trace",
			userAgent:   "midaz/1.0.0 LerianStudio",
			traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
		{
			name:        "External service propagates trace",
			userAgent:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
			traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
		{
			name:        "Missing UserAgent propagates trace",
			userAgent:   "",
			traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			tp, spanRecorder := setupTestTracer(t)
			defer func() {
				_ = tp.Shutdown(ctx)
			}()

			oldTracerProvider := otel.GetTracerProvider()
			otel.SetTracerProvider(tp)
			defer otel.SetTracerProvider(oldTracerProvider)

			tel := &tracing.Telemetry{
				TelemetryConfig: tracing.TelemetryConfig{
					LibraryName:              "test-library",
					EnableTelemetry:          true,
					TrustInboundTraceContext: true,
				},
				TracerProvider: tp,
			}

			mid := NewTelemetryMiddleware(tel)

			app := fiber.New()
			app.Use(mid.WithTelemetry(tel))

			var (
				capturedSpanContext oteltrace.SpanContext
				capturedTenant      string
			)
			app.Get("/test", func(c fiber.Ctx) error {
				capturedSpanContext = oteltrace.SpanContextFromContext(c.Context())
				capturedTenant = baggage.FromContext(c.Context()).Member("tenant.id").Value()
				return c.SendStatus(http.StatusOK)
			})

			req := httptest.NewRequest("GET", "/test", nil)

			if tt.userAgent != "" {
				req.Header.Set("User-Agent", tt.userAgent)
			}

			req.Header.Set("traceparent", tt.traceparent)
			req.Header.Set("baggage", "tenant.id=tenant-123")

			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			spans := spanRecorder.Ended()
			require.GreaterOrEqual(t, len(spans), 1, "Expected at least one span to be created")

			assert.True(t, capturedSpanContext.IsValid(), "valid traceparent must create a valid span context")
			assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", capturedSpanContext.TraceID().String(),
				"trace ID must match the valid traceparent header")
			assert.Empty(t, capturedTenant,
				"tenant.id baggage from an inbound request must NEVER propagate, even when trace context is trusted")
		})
	}
}
