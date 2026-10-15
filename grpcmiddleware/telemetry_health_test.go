//go:build unit

package grpcmiddleware

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
