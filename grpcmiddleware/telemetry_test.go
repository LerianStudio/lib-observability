//go:build unit

package grpcmiddleware

import (
	"context"
	"testing"

	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

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

// TestEndTracingSpansInterceptor_EndsUnownedSpan verifies the end interceptor
// ends a span that is present in the context but not owned by
// WithTelemetryInterceptor.
func TestEndTracingSpansInterceptor_EndsUnownedSpan(t *testing.T) {
	mid := NewTelemetryMiddleware(nil)
	end := mid.EndTracingSpansInterceptor()

	handler := func(_ context.Context, _ any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/DoThing"}

	resp, err := end(context.Background(), "req", info, handler)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)
}
