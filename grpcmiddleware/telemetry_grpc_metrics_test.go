//go:build unit

package grpcmiddleware

import (
	"context"
	"errors"
	"testing"

	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// findRPCDurationHistogram extracts a named RPC duration histogram data point
// from a ManualReader collection. Returns nil if the metric is absent (used to
// assert non-recording paths). When present it also locks the unit to seconds,
// matching the metric contract (docs/metrics-contract.md).
func findRPCDurationHistogram(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	metricName string,
) *metricdata.HistogramDataPoint[float64] {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}

			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected Float64 histogram for %s, got %T", m.Name, m.Data)
			require.NotEmpty(t, h.DataPoints, "histogram has no data points")
			require.Equal(t, "s", m.Unit, "metric unit must be seconds")

			dp := h.DataPoints[0]
			return &dp
		}
	}

	return nil
}

// TestRPCServerDurationBuckets_MatchOTelAdvisory locks the RPC bucket layout
// against the shared HTTP/RPC advisory (metric contract). Any change is
// observable from dashboards, so it MUST be a deliberate spec-tracking update.
func TestRPCServerDurationBuckets_MatchOTelAdvisory(t *testing.T) {
	expected := []float64{
		0.005, 0.01, 0.025, 0.05, 0.075,
		0.1, 0.25, 0.5, 0.75,
		1, 2.5, 5, 7.5, 10,
	}
	assert.Equal(t, expected, rpcDurationBuckets)
}

// TestWithTelemetryInterceptor_RecordsServerDurationOnSuccess verifies a
// successful unary call emits rpc.server.duration with rpc.system=grpc,
// rpc.method, and rpc.grpc.status_code=0 (OK), and NO error.type.
func TestWithTelemetryInterceptor_RecordsServerDurationOnSuccess(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.WithTelemetryInterceptor(tel)

	handler := func(_ context.Context, _ any) (any, error) {
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/DoThing"}

	_, err := interceptor(context.Background(), "req", info, handler)
	require.NoError(t, err)

	dp := findRPCDurationHistogram(t, reader, rpcServerDurationMetric)
	require.NotNil(t, dp, "expected rpc.server.duration to be recorded")
	assert.EqualValues(t, 1, dp.Count)
	assert.GreaterOrEqual(t, dp.Sum, 0.0, "duration sum must be non-negative seconds")

	system, ok := attrValue(dp.Attributes, "rpc.system")
	require.True(t, ok)
	assert.Equal(t, "grpc", system)

	method, ok := attrValue(dp.Attributes, "rpc.method")
	require.True(t, ok)
	assert.Equal(t, "/test.Service/DoThing", method)

	code, ok := dp.Attributes.Value(attribute.Key("rpc.grpc.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, int(grpccodes.OK), code.AsInt64())

	_, hasErr := dp.Attributes.Value(attribute.Key("error.type"))
	assert.False(t, hasErr, "error.type must be absent on OK responses")
}

// TestWithTelemetryInterceptor_RecordsServerDurationOnError verifies a failing
// unary call records the numeric gRPC status code and a low-cardinality
// error.type derived from that code.
func TestWithTelemetryInterceptor_RecordsServerDurationOnError(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.WithTelemetryInterceptor(tel)

	handler := func(_ context.Context, _ any) (any, error) {
		return nil, status.Error(grpccodes.NotFound, "missing")
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/GetThing"}

	_, err := interceptor(context.Background(), "req", info, handler)
	require.Error(t, err)

	dp := findRPCDurationHistogram(t, reader, rpcServerDurationMetric)
	require.NotNil(t, dp)

	code, ok := dp.Attributes.Value(attribute.Key("rpc.grpc.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, int(grpccodes.NotFound), code.AsInt64())

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok, "error.type must be set when status != OK")
	assert.Equal(t, grpccodes.NotFound.String(), errType)
}

// TestWithTelemetryInterceptor_RecordsServerDurationWithTenantID verifies the
// tenant.id label is populated from inbound gRPC metadata via the same resolver
// the span uses.
func TestWithTelemetryInterceptor_RecordsServerDurationWithTenantID(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.WithTelemetryInterceptor(tel)

	handler := func(_ context.Context, _ any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/DoThing"}

	md := metadata.New(map[string]string{"tenant-id": "acme"})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := interceptor(ctx, "req", info, handler)
	require.NoError(t, err)

	dp := findRPCDurationHistogram(t, reader, rpcServerDurationMetric)
	require.NotNil(t, dp)

	tenant, ok := attrValue(dp.Attributes, "tenant.id")
	require.True(t, ok, "tenant.id must be present when tenant-id metadata is supplied")
	assert.Equal(t, "acme", tenant)
}

// TestWithTelemetryInterceptor_NilMetricsFactoryDoesNotRecord verifies the
// server metric is gated on MetricsFactory presence, matching the HTTP path.
func TestWithTelemetryInterceptor_NilMetricsFactoryDoesNotRecord(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{LibraryName: "test-library"},
		MeterProvider:   mp,
		// MetricsFactory intentionally nil.
	}

	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.WithTelemetryInterceptor(tel)

	handler := func(_ context.Context, _ any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/DoThing"}

	_, err := interceptor(context.Background(), "req", info, handler)
	require.NoError(t, err)

	assert.Nil(t, findRPCDurationHistogram(t, reader, rpcServerDurationMetric),
		"nil MetricsFactory must not record rpc.server.duration")
}

// TestUnaryClientInterceptor_RecordsClientDurationOnSuccess verifies the
// client interceptor emits rpc.client.duration with rpc.system=grpc,
// rpc.method, rpc.grpc.status_code=0 and NO tenant.id (client side omits it).
func TestUnaryClientInterceptor_RecordsClientDurationOnSuccess(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.UnaryClientInterceptor(tel)

	invoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		return nil
	}

	err := interceptor(context.Background(), "/test.Service/DoThing", "req", "reply", nil, invoker)
	require.NoError(t, err)

	dp := findRPCDurationHistogram(t, reader, rpcClientDurationMetric)
	require.NotNil(t, dp, "expected rpc.client.duration to be recorded")
	assert.EqualValues(t, 1, dp.Count)

	system, ok := attrValue(dp.Attributes, "rpc.system")
	require.True(t, ok)
	assert.Equal(t, "grpc", system)

	method, ok := attrValue(dp.Attributes, "rpc.method")
	require.True(t, ok)
	assert.Equal(t, "/test.Service/DoThing", method)

	code, ok := dp.Attributes.Value(attribute.Key("rpc.grpc.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, int(grpccodes.OK), code.AsInt64())

	_, hasErr := dp.Attributes.Value(attribute.Key("error.type"))
	assert.False(t, hasErr)

	_, hasTenant := dp.Attributes.Value(attribute.Key("tenant.id"))
	assert.False(t, hasTenant, "client metric must never carry tenant.id")
}

// TestUnaryClientInterceptor_RecordsClientDurationOnError verifies the client
// interceptor records the numeric status code and a numeric-code error.type
// when the invoker returns an error.
func TestUnaryClientInterceptor_RecordsClientDurationOnError(t *testing.T) {
	tel, reader := newMetricsHarness(t)

	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.UnaryClientInterceptor(tel)

	invoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		return status.Error(grpccodes.Unavailable, "down")
	}

	err := interceptor(context.Background(), "/test.Service/DoThing", "req", "reply", nil, invoker)
	require.Error(t, err)

	dp := findRPCDurationHistogram(t, reader, rpcClientDurationMetric)
	require.NotNil(t, dp)

	code, ok := dp.Attributes.Value(attribute.Key("rpc.grpc.status_code"))
	require.True(t, ok)
	assert.EqualValues(t, int(grpccodes.Unavailable), code.AsInt64())

	errType, ok := attrValue(dp.Attributes, "error.type")
	require.True(t, ok)
	assert.Equal(t, grpccodes.Unavailable.String(), errType)
}

// TestUnaryClientInterceptor_InjectsTraceContext verifies the client
// interceptor propagates trace context into outgoing gRPC metadata, so
// downstream services join the trace rather than starting a new root. A real
// span is started (and the global W3C propagator installed) so there is a valid
// SpanContext for InjectGRPCContext to serialize.
func TestUnaryClientInterceptor_InjectsTraceContext(t *testing.T) {
	tel, _, _ := newTelemetryHarness(t)

	tp, _ := setupTestTracer(t) // installs the global TraceContext propagator
	ctx, span := tp.Tracer("client-test").Start(context.Background(), "caller")
	defer span.End()

	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.UnaryClientInterceptor(tel)

	var outgoingMD metadata.MD

	invoker := func(ictx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		outgoingMD, _ = metadata.FromOutgoingContext(ictx)
		return nil
	}

	err := interceptor(ctx, "/test.Service/DoThing", "req", "reply", nil, invoker)
	require.NoError(t, err)

	require.NotNil(t, outgoingMD, "invoker must observe outgoing metadata")
	assert.NotEmpty(t, outgoingMD.Get("traceparent"),
		"client interceptor must inject the W3C traceparent for propagation")
}

// TestUnaryClientInterceptor_NilTelemetryIsNoOp verifies the client interceptor
// is safe with nil telemetry and still invokes the downstream call.
func TestUnaryClientInterceptor_NilTelemetryIsNoOp(t *testing.T) {
	mid := NewTelemetryMiddleware(nil)
	interceptor := mid.UnaryClientInterceptor(nil)

	called := false
	invoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		called = true
		return errors.New("downstream")
	}

	err := interceptor(context.Background(), "/test.Service/DoThing", "req", "reply", nil, invoker)
	require.Error(t, err)
	assert.True(t, called, "invoker must be called even when telemetry is nil")
}
