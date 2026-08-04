//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/LerianStudio/lib-observability/v2/metrics"
	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// setupTestTracer sets up a test tracer provider and returns it along with a span recorder.
func setupTestTracer(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spanRecorder),
	)

	oldPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
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

				expectedPath := route

				spanFound := false
				for _, span := range spans {
					if span.Name() == tt.method+" "+expectedPath {
						spanFound = true
						break
					}
				}
				assert.True(t, spanFound, "Expected span with name %s not found", tt.method+" "+expectedPath)
			} else if tt.swaggerPath || tt.nilTelemetry {
				assert.Empty(t, spans, "Expected no spans for swagger path or nil telemetry")
			}
		})
	}
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

// TestGetMetricsCollectionInterval tests the getMetricsCollectionInterval function.
func TestGetMetricsCollectionInterval(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected time.Duration
	}{
		{
			name:     "default when not set",
			envValue: "",
			expected: DefaultMetricsCollectionInterval,
		},
		{
			name:     "valid duration in seconds",
			envValue: "10s",
			expected: 10 * time.Second,
		},
		{
			name:     "valid duration in milliseconds",
			envValue: "500ms",
			expected: 500 * time.Millisecond,
		},
		{
			name:     "valid duration in minutes",
			envValue: "1m",
			expected: 1 * time.Minute,
		},
		{
			name:     "invalid format falls back to default",
			envValue: "invalid",
			expected: DefaultMetricsCollectionInterval,
		},
		{
			name:     "zero value falls back to default",
			envValue: "0s",
			expected: DefaultMetricsCollectionInterval,
		},
		{
			name:     "negative value falls back to default",
			envValue: "-5s",
			expected: DefaultMetricsCollectionInterval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv("METRICS_COLLECTION_INTERVAL", tt.envValue)
			} else {
				t.Setenv("METRICS_COLLECTION_INTERVAL", "")
			}

			result := getMetricsCollectionInterval()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func resetMetricsCollectorState() {
	metricsCollectorMu.Lock()
	defer metricsCollectorMu.Unlock()

	if metricsCollectorStarted && metricsCollectorShutdown != nil {
		close(metricsCollectorShutdown)
		time.Sleep(50 * time.Millisecond)
	}

	metricsCollectorShutdown = nil
	metricsCollectorStarted = false
	metricsCollectorOnce = &sync.Once{}
	metricsCollectorInitErr = nil
}

func TestEnsureMetricsCollector_ReturnsErrorWhenMetricsFactoryNil(t *testing.T) {
	resetMetricsCollectorState()
	t.Cleanup(resetMetricsCollectorState)

	mid := &TelemetryMiddleware{Telemetry: &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library", EnableTelemetry: true},
		MeterProvider:   sdkmetric.NewMeterProvider(),
	}}

	err := mid.ensureMetricsCollector()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MetricsFactory is nil")
	assert.False(t, metricsCollectorStarted)
}

func TestEnsureMetricsCollector_NoMeterProviderReturnsNil(t *testing.T) {
	resetMetricsCollectorState()
	t.Cleanup(resetMetricsCollectorState)

	mid := &TelemetryMiddleware{Telemetry: &tracing.Telemetry{}}
	require.NoError(t, mid.ensureMetricsCollector())
	assert.False(t, metricsCollectorStarted)
}

func TestStopMetricsCollector_AllowsRestart(t *testing.T) {
	resetMetricsCollectorState()
	t.Cleanup(resetMetricsCollectorState)

	mid := &TelemetryMiddleware{Telemetry: &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library", EnableTelemetry: true},
		MeterProvider:   sdkmetric.NewMeterProvider(),
		MetricsFactory:  metrics.NewNopFactory(),
	}}

	require.NoError(t, mid.ensureMetricsCollector())
	assert.True(t, metricsCollectorStarted)

	StopMetricsCollector()
	assert.False(t, metricsCollectorStarted)

	require.NoError(t, mid.ensureMetricsCollector())
	assert.True(t, metricsCollectorStarted)
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
		ctx := tracing.ExtractHTTPContext(c.Context(), c)

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

// TestWithTelemetryConditionalTracePropagation tests the conditional trace propagation based on UserAgent.
func TestWithTelemetryConditionalTracePropagation(t *testing.T) {
	tests := []struct {
		name                 string
		userAgent            string
		traceparent          string
		shouldPropagateTrace bool
		description          string
	}{
		{
			name:                 "Internal Lerian service - should propagate trace",
			userAgent:            "midaz/1.0.0 LerianStudio",
			traceparent:          "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			shouldPropagateTrace: true,
			description:          "Internal service with valid UserAgent pattern should propagate trace context",
		},
		{
			name:                 "External service - should NOT propagate trace",
			userAgent:            "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
			traceparent:          "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			shouldPropagateTrace: false,
			description:          "External browser UserAgent should create new root span",
		},
		{
			name:                 "No UserAgent - should NOT propagate trace",
			userAgent:            "",
			traceparent:          "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			shouldPropagateTrace: false,
			description:          "Missing UserAgent should create new root span",
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
			app.Use(mid.WithTelemetry(tel))

			var capturedSpanContext oteltrace.SpanContext
			app.Get("/test", func(c fiber.Ctx) error {
				capturedSpanContext = oteltrace.SpanContextFromContext(c.Context())
				return c.SendStatus(http.StatusOK)
			})

			req := httptest.NewRequest("GET", "/test", nil)

			if tt.userAgent != "" {
				req.Header.Set("User-Agent", tt.userAgent)
			}

			req.Header.Set("traceparent", tt.traceparent)

			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			spans := spanRecorder.Ended()
			require.GreaterOrEqual(t, len(spans), 1, "Expected at least one span to be created")

			if tt.shouldPropagateTrace {
				assert.True(t, capturedSpanContext.IsValid(), "Span context should be valid for internal services")
				assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", capturedSpanContext.TraceID().String(),
					"Trace ID should match the traceparent header for internal services")
			} else {
				require.True(t, capturedSpanContext.IsValid(), "Expected middleware to attach a valid span context")
				assert.NotEqual(t, "4bf92f3577b34da6a3ce929d0e0e4736", capturedSpanContext.TraceID().String(),
					"Trace ID should be different from traceparent header for external services")
			}
		})
	}
}

// TestGetGRPCUserAgent tests the getGRPCUserAgent helper function.
func TestGetGRPCUserAgent(t *testing.T) {
	tests := []struct {
		name          string
		setupMetadata func() context.Context
		expectedUA    string
		description   string
	}{
		{
			name: "Valid user-agent in metadata",
			setupMetadata: func() context.Context {
				md := metadata.Pairs("user-agent", "midaz/1.0.0 LerianStudio")
				return metadata.NewIncomingContext(context.Background(), md)
			},
			expectedUA:  "midaz/1.0.0 LerianStudio",
			description: "Should extract user-agent from gRPC metadata",
		},
		{
			name: "No metadata in context",
			setupMetadata: func() context.Context {
				return context.Background()
			},
			expectedUA:  "",
			description: "Should return empty string when no metadata present",
		},
		{
			name: "Metadata without user-agent",
			setupMetadata: func() context.Context {
				md := metadata.Pairs("authorization", "Bearer token")
				return metadata.NewIncomingContext(context.Background(), md)
			},
			expectedUA:  "",
			description: "Should return empty string when user-agent key not present",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setupMetadata()
			result := getGRPCUserAgent(ctx)
			assert.Equal(t, tt.expectedUA, result, tt.description)
		})
	}
}

// TestWithTelemetryInterceptorConditionalTracePropagation tests conditional trace propagation in gRPC interceptor.
func TestWithTelemetryInterceptorConditionalTracePropagation(t *testing.T) {
	tests := []struct {
		name                 string
		userAgent            string
		traceparent          string
		shouldPropagateTrace bool
		description          string
	}{
		{
			name:                 "Internal Lerian service via gRPC - should propagate trace",
			userAgent:            "midaz/1.0.0 LerianStudio",
			traceparent:          "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			shouldPropagateTrace: true,
			description:          "Internal gRPC service should propagate trace context",
		},
		{
			name:                 "External gRPC client - should NOT propagate trace",
			userAgent:            "grpc-go/1.50.0",
			traceparent:          "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			shouldPropagateTrace: false,
			description:          "External gRPC client should create new root span",
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
			interceptor := mid.WithTelemetryInterceptor(tel)

			md := metadata.New(map[string]string{})
			if tt.userAgent != "" {
				md.Set("user-agent", tt.userAgent)
			}
			if tt.traceparent != "" {
				md.Set("traceparent", tt.traceparent)
			}
			ctx = metadata.NewIncomingContext(ctx, md)

			var capturedSpanContext oteltrace.SpanContext
			handler := func(ctx context.Context, req any) (any, error) {
				capturedSpanContext = oteltrace.SpanContextFromContext(ctx)
				return "response", nil
			}

			info := &grpc.UnaryServerInfo{
				FullMethod: "/test.Service/Method",
			}

			_, err := interceptor(ctx, "request", info, handler)
			require.NoError(t, err)

			spans := spanRecorder.Ended()
			require.GreaterOrEqual(t, len(spans), 1, "Expected at least one span to be created")

			if tt.shouldPropagateTrace {
				assert.True(t, capturedSpanContext.IsValid(), "Span context should be valid for internal services")
				assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", capturedSpanContext.TraceID().String(),
					"Trace ID should match the traceparent for internal gRPC services")
			} else {
				require.True(t, capturedSpanContext.IsValid(), "Expected middleware to attach a valid span context")
				assert.NotEqual(t, "4bf92f3577b34da6a3ce929d0e0e4736", capturedSpanContext.TraceID().String(),
					"Trace ID should be different from traceparent for external services")
			}
		})
	}
}
