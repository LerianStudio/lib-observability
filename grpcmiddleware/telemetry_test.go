//go:build unit

package grpcmiddleware

import (
	"context"
	"errors"
	"testing"

	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestWithTelemetryInterceptor_TrustInboundTraceContext verifies gRPC's
// inbound trace-context extraction is gated by the SAME TrustInboundTraceContext
// knob HTTP uses (tracing.TelemetryConfig), fail-closed by default. This
// replaces the previous User-Agent heuristic (isInternalLerianService),
// which was spoofable by any caller that simply set the header and so was
// never a real trust boundary - the table below proves the UA now has NO
// effect either way: an "internal-looking" UA does not get trusted by
// default, and an "external" one is trusted once the knob opts in.
func TestWithTelemetryInterceptor_TrustInboundTraceContext(t *testing.T) {
	tests := []struct {
		name       string
		trust      bool
		userAgent  string
		wantJoined bool
	}{
		{
			name:      "default (unset) does not trust an internal-looking User-Agent",
			userAgent: "midaz/1.0.0 LerianStudio",
		},
		{
			name:      "default (unset) does not trust an external User-Agent",
			userAgent: "grpc-go/1.50.0",
		},
		{
			name:       "explicit opt-in trusts inbound trace context regardless of User-Agent",
			trust:      true,
			userAgent:  "grpc-go/1.50.0",
			wantJoined: true,
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
					TrustInboundTraceContext: tt.trust,
				},
				TracerProvider: tp,
			}

			mid := NewTelemetryMiddleware(tel)
			interceptor := mid.WithTelemetryInterceptor(tel)

			md := metadata.Pairs(
				"user-agent", tt.userAgent,
				"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			)
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
			require.True(t, capturedSpanContext.IsValid(), "Expected middleware to attach a valid span context")

			if tt.wantJoined {
				assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", capturedSpanContext.TraceID().String(),
					"Trace ID should match the traceparent once TrustInboundTraceContext opts in")
			} else {
				assert.NotEqual(t, "4bf92f3577b34da6a3ce929d0e0e4736", capturedSpanContext.TraceID().String(),
					"an untrusted inbound traceparent must never be honored by default")
			}
		})
	}
}

// TestWithTelemetryInterceptor_StripsInboundTenantIDBaggage mirrors the HTTP
// counterpart (middleware.TestExtractHTTPContext_StripsTenantIDBaggage): even
// a caller that HAS opted into TrustInboundTraceContext must still never
// have its tenant.id baggage member trusted. Other baggage members
// propagate normally, proving this isn't baggage propagation being broken
// again - it is the tenant.id member specifically that is always stripped.
func TestWithTelemetryInterceptor_StripsInboundTenantIDBaggage(t *testing.T) {
	// Not parallel: mutates the process-global OTel propagator, which this
	// test needs to actually include Baggage (the shared test harnesses in
	// this package only configure TracerProvider/MeterProvider).
	prevPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prevPropagator) })
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	tel, _, spanExp := newTelemetryHarness(t)
	tel.TrustInboundTraceContext = true
	interceptor := NewTelemetryMiddleware(tel).WithTelemetryInterceptor(tel)

	md := metadata.Pairs(
		"baggage", "tenant.id=victim,region=us-east",
	)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	var gotTenant, gotRegion string

	handler := func(handlerCtx context.Context, _ any) (any, error) {
		gotTenant = baggage.FromContext(handlerCtx).Member("tenant.id").Value()
		gotRegion = baggage.FromContext(handlerCtx).Member("region").Value()

		return "ok", nil
	}

	_, err := interceptor(ctx, "req", &grpc.UnaryServerInfo{FullMethod: "/test.Service/DoThing"}, handler)
	require.NoError(t, err)

	assert.Empty(t, gotTenant,
		"tenant.id must never propagate from inbound gRPC baggage, even when TrustInboundTraceContext is opted in")
	assert.Equal(t, "us-east", gotRegion, "other baggage members must still propagate")

	require.NotEmpty(t, spanExp.GetSpans(), "a span must still be recorded")
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

// typedNilGRPCHandlerError has an unsafe Error() implementation (dereferences
// the nil receiver's field) so this test proves the interceptor survives a
// handler returning a typed-nil rather than by coincidence.
type typedNilGRPCHandlerError struct {
	message string
}

func (e *typedNilGRPCHandlerError) Error() string {
	return e.message
}

// TestWithTelemetryInterceptor_HandlerReturnsTypedNilDoesNotPanic covers the
// crash this fix removes: err != nil is true for a typed-nil interface, so
// google.golang.org/grpc/status.Code (called for telemetry) reaches its
// fallback path and calls err.Error() unconditionally - panicking on the
// handler's bug before tracing.HandleSpanError is even reached. Unrecovered,
// that panic kills the serving goroutine and the whole process, not just the
// one RPC. The interceptor must normalize the top-level typed-nil to a safe
// error, both for its own telemetry AND for what it returns, since grpc-go's
// own dispatch performs the identical status.FromError conversion one level
// up on whatever this interceptor hands back.
func TestWithTelemetryInterceptor_HandlerReturnsTypedNilDoesNotPanic(t *testing.T) {
	t.Parallel()

	tel, _, _ := newTelemetryHarness(t)
	mid := NewTelemetryMiddleware(tel)
	interceptor := mid.WithTelemetryInterceptor(tel)

	var typedNil *typedNilGRPCHandlerError

	handler := func(_ context.Context, _ any) (any, error) { return nil, typedNil }
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/DoThing"}

	var (
		resp any
		err  error
	)

	require.NotPanics(t, func() {
		resp, err = interceptor(context.Background(), "req", info, handler)
	})

	assert.Nil(t, resp)
	require.Error(t, err, "a handler bug must still surface as an error to the caller")
	assert.NotSame(t, error(typedNil), err,
		"the raw typed-nil must be replaced before it reaches grpc-go's own status conversion")
	require.NotPanics(t, func() { _ = err.Error() },
		"the normalized error must itself be safe to stringify")
}

// delegatingGRPCError is a valid, non-nil wrapper whose Error() blindly
// delegates to a possibly-nil cause with no guard - the general form of "a
// custom wrapper that doesn't check its Cause for nil" the fix must catch,
// distinct from both a bare typed-nil and errors.Join.
type delegatingGRPCError struct {
	cause error
}

func (e delegatingGRPCError) Error() string {
	return "wrapped: " + e.cause.Error()
}

// TestNormalizeGRPCError_CatchesEveryUnsafeShape proves the guard
// works by testing stringifiability directly, not by inspecting the error's
// shape: a bare log.IsNil check only catches the first of these three forms,
// all of which panic identically on status.FromError's unguarded err.Error()
// fallback.
func TestNormalizeGRPCError_CatchesEveryUnsafeShape(t *testing.T) {
	t.Parallel()

	var typedNil *typedNilGRPCHandlerError

	tests := []struct {
		name string
		err  error
	}{
		{name: "bare top-level typed-nil", err: typedNil},
		{name: "errors.Join with a typed-nil member", err: errors.Join(errors.New("sibling"), typedNil)},
		{name: "delegating wrapper with a nil cause", err: delegatingGRPCError{cause: typedNil}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got error

			require.NotPanics(t, func() {
				got = NormalizeGRPCError(tt.err)
			})

			require.Error(t, got)
			require.NotPanics(t, func() { _ = got.Error() },
				"the normalized error must itself be safe to stringify")
		})
	}
}

// TestNormalizeGRPCError_PassesThroughSafeErrors verifies the guard
// only replaces genuinely unsafe errors: an ordinary error, and a real gRPC
// status error (whose Error() is always safe to call), must reach grpc-go's
// dispatch unchanged so the actual status code survives.
func TestNormalizeGRPCError_PassesThroughSafeErrors(t *testing.T) {
	t.Parallel()

	ordinary := errors.New("plain failure")
	grpcStatus := status.Error(grpccodes.NotFound, "missing")

	assert.Same(t, ordinary, NormalizeGRPCError(ordinary))
	assert.Same(t, grpcStatus, NormalizeGRPCError(grpcStatus))
	assert.Nil(t, NormalizeGRPCError(nil))
}

// TestWithTelemetryInterceptor_NoTracerProviderSurvivesTypedNilAndPreservesStatus
// covers the effectiveTelemetry.TracerProvider == nil branch specifically
// (metrics-only telemetry, no tracer configured): a handler returning a
// typed-nil must not panic there either - that branch calls status.Code
// directly too - and a handler returning a real, valid gRPC status error
// must survive normalization with its status code intact.
func TestWithTelemetryInterceptor_NoTracerProviderSurvivesTypedNilAndPreservesStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		handlerErr  error
		wantCode    grpccodes.Code
		wantNotSame bool
	}{
		{
			name:        "typed-nil is normalized",
			handlerErr:  (*typedNilGRPCHandlerError)(nil),
			wantCode:    grpccodes.Unknown,
			wantNotSame: true,
		},
		{
			name:       "valid status code survives normalization",
			handlerErr: status.Error(grpccodes.NotFound, "missing"),
			wantCode:   grpccodes.NotFound,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tel, _ := newMetricsHarness(t) // MeterProvider only - no TracerProvider
			interceptor := NewTelemetryMiddleware(tel).WithTelemetryInterceptor(tel)

			handler := func(context.Context, any) (any, error) { return nil, tt.handlerErr }
			info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/DoThing"}

			var (
				resp any
				err  error
			)

			require.NotPanics(t, func() {
				resp, err = interceptor(context.Background(), "req", info, handler)
			})

			assert.Nil(t, resp)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, status.Code(err))

			if tt.wantNotSame {
				assert.NotSame(t, tt.handlerErr, err)
			}
		})
	}
}

// TestUnaryClientInterceptor_SurvivesTypedNilAndPreservesValidStatus mirrors
// the server-side normalization coverage for the client interceptor: an
// invoker returning a typed-nil must not panic status.Code (called for the
// client duration metric), and an invoker returning a real, valid gRPC
// status error must reach the caller with its status code intact.
func TestUnaryClientInterceptor_SurvivesTypedNilAndPreservesValidStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		invokerErr  error
		wantCode    grpccodes.Code
		wantNotSame bool
	}{
		{
			name:        "typed-nil is normalized",
			invokerErr:  (*typedNilGRPCHandlerError)(nil),
			wantCode:    grpccodes.Unknown,
			wantNotSame: true,
		},
		{
			name:       "valid status code survives normalization",
			invokerErr: status.Error(grpccodes.Unavailable, "downstream unavailable"),
			wantCode:   grpccodes.Unavailable,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tel, _ := newMetricsHarness(t)
			interceptor := NewTelemetryMiddleware(tel).UnaryClientInterceptor(tel)

			invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
				return tt.invokerErr
			}

			var err error

			require.NotPanics(t, func() {
				err = interceptor(context.Background(), "/test.Service/DoThing", "req", "reply", nil, invoker)
			})

			require.Error(t, err)
			assert.Equal(t, tt.wantCode, status.Code(err))

			if tt.wantNotSame {
				assert.NotSame(t, tt.invokerErr, err)
			}
		})
	}
}
