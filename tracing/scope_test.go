//go:build unit

package tracing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	observability "github.com/LerianStudio/lib-observability/v4"
	"github.com/LerianStudio/lib-observability/v4/grpcmiddleware"
	"github.com/LerianStudio/lib-observability/v4/internal/buildmeta"
	"github.com/LerianStudio/lib-observability/v4/messagingobs"
	"github.com/LerianStudio/lib-observability/v4/metrics"
	"github.com/LerianStudio/lib-observability/v4/middleware"
	"github.com/LerianStudio/lib-observability/v4/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
)

const (
	// consumerLibraryName is the service's TelemetryConfig.LibraryName: the
	// scope of the signals the service emits through this library, and never
	// the scope of a signal this library emits itself.
	consumerLibraryName = "github.com/LerianStudio/some-consumer-service"
	// consumerServiceName is the service's ServiceName; never part of any scope.
	consumerServiceName = "some-consumer-service"
)

type scopeHarness struct {
	tel      *tracing.Telemetry
	recorder *tracetest.SpanRecorder
	reader   *sdkmetric.ManualReader
}

func newScopeHarness(t *testing.T) *scopeHarness {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{
			LibraryName:     consumerLibraryName,
			ServiceName:     consumerServiceName,
			EnableTelemetry: true,
		},
		TracerProvider: tp,
		MeterProvider:  mp,
		// A real factory, not the nop one: a nop factory emits nothing, so it
		// would hide whichever scope the factory actually carries.
		MetricsFactory: newRealFactory(t, mp),
	}

	return &scopeHarness{tel: tel, recorder: recorder, reader: reader}
}

func newRealFactory(t *testing.T, mp *sdkmetric.MeterProvider) *metrics.MetricsFactory {
	t.Helper()

	f, err := metrics.NewMetricsFactory(mp.Meter(consumerLibraryName), nil)
	require.NoError(t, err)

	return f
}

// metricScope returns the instrumentation scope of the ScopeMetrics that holds
// the named instrument.
func (h *scopeHarness) metricScope(t *testing.T, metricName string) (name, version string) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, h.reader.Collect(context.Background(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == metricName {
				return sm.Scope.Name, sm.Scope.Version
			}
		}
	}

	t.Fatalf("metric %q was never recorded", metricName)

	return "", ""
}

func driveHTTP(t *testing.T, h *scopeHarness) {
	t.Helper()

	app := fiber.New()
	app.Use(middleware.NewTelemetryMiddleware(h.tel).WithTelemetry(h.tel))
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendString("ok") })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ok", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func driveGRPCServer(t *testing.T, h *scopeHarness) {
	t.Helper()

	interceptor := grpcmiddleware.NewTelemetryMiddleware(h.tel).WithTelemetryInterceptor(h.tel)

	_, err := interceptor(context.Background(), "req",
		&grpc.UnaryServerInfo{FullMethod: "/pkg.Svc/Method"},
		func(context.Context, any) (any, error) { return "resp", nil },
	)
	require.NoError(t, err)
}

func driveGRPCClient(t *testing.T, h *scopeHarness) {
	t.Helper()

	interceptor := grpcmiddleware.NewTelemetryMiddleware(h.tel).UnaryClientInterceptor(h.tel)

	err := interceptor(context.Background(), "/pkg.Svc/Method", "req", "reply", nil,
		func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error { return nil },
	)
	require.NoError(t, err)
}

func drivePublisher(t *testing.T, h *scopeHarness) {
	t.Helper()

	_, _, finish := messagingobs.NewPublisher(h.tel).Produce(context.Background(),
		messagingobs.ProduceParams{DestinationTemplate: "tx.{tenant}", OperationName: "publish"})
	finish(nil)
}

func driveConsumer(t *testing.T, h *scopeHarness) {
	t.Helper()

	_, finish := messagingobs.NewConsumer(h.tel).Consume(context.Background(),
		messagingobs.ConsumeParams{DestinationTemplate: "tx.{tenant}", OperationName: "process"})
	finish(nil)
}

// TestLibrarySignalsCarryLibraryScope drives every emitter this library owns
// against in-memory providers and asserts that the spans and instruments it
// creates itself are attributed to lib-observability's module path and version
// (FC-7) - never to the LibraryName the service configured for its own signals.
func TestLibrarySignalsCarryLibraryScope(t *testing.T) {
	t.Parallel()

	wantName, wantVersion := buildmeta.Scope()

	tests := []struct {
		name       string
		drive      func(*testing.T, *scopeHarness)
		metricName string
		wantSpans  bool
	}{
		{"http middleware", driveHTTP, "http.server.request.duration", true},
		{"grpc server interceptor", driveGRPCServer, "rpc.server.duration", true},
		{"grpc client interceptor", driveGRPCClient, "rpc.client.duration", false},
		{"messaging publisher", drivePublisher, "messaging.client.operation.duration", true},
		{"messaging consumer", driveConsumer, "messaging.process.duration", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newScopeHarness(t)
			tt.drive(t, h)

			gotName, gotVersion := h.metricScope(t, tt.metricName)
			assert.Equal(t, wantName, gotName, "%s must carry the library module path", tt.metricName)
			assert.Equal(t, wantVersion, gotVersion, "%s must carry the library module version", tt.metricName)

			spans := h.recorder.Ended()
			if !tt.wantSpans {
				assert.Empty(t, spans, "the client interceptor must not open a span")

				return
			}

			require.NotEmpty(t, spans, "the emitter must have produced a span")

			for _, span := range spans {
				assert.Equal(t, wantName, span.InstrumentationScope().Name)
				assert.Equal(t, wantVersion, span.InstrumentationScope().Version)
				assert.NotEqual(t, consumerLibraryName, span.InstrumentationScope().Name,
					"the service's LibraryName must never reach a library signal")
			}
		})
	}
}

// TestServiceCarriersUseServiceScope is the mirror of the test above: the
// tracer this library puts on the request context belongs to the service code
// that pulls it out, so a span a handler opens must be attributed to the
// service's LibraryName - not to this library, whose scope version changes on
// every release.
func TestServiceCarriersUseServiceScope(t *testing.T) {
	t.Parallel()

	libName, _ := buildmeta.Scope()

	assertServiceTracer := func(t *testing.T, h *scopeHarness, ctx context.Context) {
		t.Helper()

		_, tracer, _, _ := observability.NewTrackingFromContext(ctx)
		require.NotNil(t, tracer)

		_, span := tracer.Start(context.Background(), "handler-span")
		span.End()

		var handlerSpan sdktrace.ReadOnlySpan

		for _, s := range h.recorder.Ended() {
			if s.Name() == "handler-span" {
				handlerSpan = s
			}
		}

		require.NotNil(t, handlerSpan, "the span opened through the context tracer must be recorded")
		assert.Equal(t, consumerLibraryName, handlerSpan.InstrumentationScope().Name,
			"a span the service opens must be attributed to the service's LibraryName")
		assert.NotEqual(t, libName, handlerSpan.InstrumentationScope().Name,
			"a library upgrade must not re-attribute or re-version a service span")
		assert.Empty(t, handlerSpan.InstrumentationScope().Version,
			"a service span carries no library version")
	}

	t.Run("http request context", func(t *testing.T) {
		t.Parallel()

		h := newScopeHarness(t)

		var captured context.Context

		app := fiber.New()
		app.Use(middleware.NewTelemetryMiddleware(h.tel).WithTelemetry(h.tel))
		app.Get("/ok", func(c fiber.Ctx) error {
			captured = c.Context()

			return c.SendString("ok")
		})

		_, err := app.Test(httptest.NewRequest(http.MethodGet, "/ok", nil))
		require.NoError(t, err)
		require.NotNil(t, captured)

		assertServiceTracer(t, h, captured)
	})

	t.Run("grpc handler context", func(t *testing.T) {
		t.Parallel()

		h := newScopeHarness(t)
		interceptor := grpcmiddleware.NewTelemetryMiddleware(h.tel).WithTelemetryInterceptor(h.tel)

		var captured context.Context

		_, err := interceptor(context.Background(), "req",
			&grpc.UnaryServerInfo{FullMethod: "/pkg.Svc/Method"},
			func(ctx context.Context, _ any) (any, error) {
				captured = ctx

				return "resp", nil
			},
		)
		require.NoError(t, err)
		require.NotNil(t, captured)

		assertServiceTracer(t, h, captured)
	})
}
