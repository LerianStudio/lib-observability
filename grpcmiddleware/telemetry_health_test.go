//go:build unit

package grpcmiddleware

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
)

// TestWithTelemetryInterceptor_HealthCheckOpensNoSpan pins that methods of the
// standard gRPC health service get no server span, while every other method
// still does. Both keep rpc.server.duration.
func TestWithTelemetryInterceptor_HealthCheckOpensNoSpan(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		wantSpans int
	}{
		{name: "health Check", method: "/grpc.health.v1.Health/Check", wantSpans: 0},
		{name: "health Watch", method: "/grpc.health.v1.Health/Watch", wantSpans: 0},
		{name: "regular method", method: "/test.Service/DoThing", wantSpans: 1},
		{name: "method that only starts like health", method: "/grpc.health.v1.HealthExtra/Check", wantSpans: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tel, reader, spanExp := newTelemetryHarness(t)
			interceptor := NewTelemetryMiddleware(tel).WithTelemetryInterceptor(tel)

			handler := func(_ context.Context, _ any) (any, error) { return "ok", nil }
			info := &grpc.UnaryServerInfo{FullMethod: tt.method}

			_, err := interceptor(context.Background(), "req", info, handler)
			require.NoError(t, err)

			assert.Len(t, spanExp.GetSpans(), tt.wantSpans)

			dp := findRPCDurationHistogram(t, reader, rpcServerDurationMetric)
			require.NotNil(t, dp, "rpc.server.duration must be recorded either way")
			assert.EqualValues(t, 1, dp.Count)
		})
	}
}

// TestEndTracingSpansInterceptor_HealthCheckLeavesUpstreamSpanOpen pins that,
// in a WithTelemetryInterceptor -> EndTracingSpansInterceptor chain, a health
// check (which opens no server span) does not end a span that something
// upstream put in the context. That span belongs to its owner, who ends it.
func TestEndTracingSpansInterceptor_HealthCheckLeavesUpstreamSpanOpen(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		tracingOff bool
	}{
		{name: "health check", method: "/grpc.health.v1.Health/Check"},
		{name: "health check, tracing off", method: "/grpc.health.v1.Health/Check", tracingOff: true},
		{name: "regular method", method: "/test.Service/DoThing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tel, _, _ := newTelemetryHarness(t)
			if tt.tracingOff {
				tel.TracerProvider = nil
			}

			mid := NewTelemetryMiddleware(tel)
			start := mid.WithTelemetryInterceptor(tel)
			end := mid.EndTracingSpansInterceptor()

			upstreamTP := sdktrace.NewTracerProvider()
			t.Cleanup(func() { _ = upstreamTP.Shutdown(context.Background()) })

			ctx, upstream := upstreamTP.Tracer("upstream").Start(context.Background(), "upstream")
			defer upstream.End()

			info := &grpc.UnaryServerInfo{FullMethod: tt.method}
			handler := func(_ context.Context, _ any) (any, error) { return "ok", nil }
			chained := func(ctx context.Context, req any) (any, error) {
				return end(ctx, req, info, handler)
			}

			_, err := start(ctx, "req", info, chained)
			require.NoError(t, err)

			assert.True(t, upstream.IsRecording(),
				"the interceptors must not end a span they did not open")
		})
	}
}
