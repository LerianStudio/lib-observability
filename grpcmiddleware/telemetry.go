// Package grpcmiddleware provides gRPC telemetry interceptors (server and
// client) that integrate with the lib-observability tracing and metrics
// packages.
//
// It is deliberately Fiber-free: none of the code in this package imports
// github.com/gofiber/fiber, so applications still on Fiber v2 can wire up gRPC
// tracing and the rpc.server.duration / rpc.client.duration metrics without
// pulling in Fiber v3. The HTTP counterpart lives in the middleware package,
// and both share the single process-wide system-metrics collector via
// telemetrycore.
package grpcmiddleware

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	observability "github.com/LerianStudio/lib-observability/v2"
	constant "github.com/LerianStudio/lib-observability/v2/constants"
	"github.com/LerianStudio/lib-observability/v2/log"
	"github.com/LerianStudio/lib-observability/v2/telemetrycore"
	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// rpcServerDurationMetric / rpcClientDurationMetric are the metric names for
// gRPC server- and client-side call duration. Recorded as Float64 histograms in
// seconds.
//
// Naming note (see docs/metrics-contract.md): the OTel RPC semconv train is
// mid-migration between the experimental `rpc.server.duration` and the RC
// `rpc.server.call.duration`. We intentionally keep the experimental names so
// the metric aligns with the span attributes this library already emits
// (`rpc.grpc.status_code`), giving operators one consistent RPC vocabulary
// across traces and metrics. Revisit in lockstep when the span attributes
// migrate to the RC names.
const (
	rpcServerDurationMetric = "rpc.server.duration"
	rpcClientDurationMetric = "rpc.client.duration"
)

// rpcSystemGRPC is the rpc.system attribute value for gRPC calls.
const rpcSystemGRPC = "grpc"

// metadataID is the gRPC metadata key that carries the request context identifier.
const metadataID = "metadata_id"

// ErrContextNotFound is returned when a required context is nil.
var ErrContextNotFound = errors.New("context not found")

// rpcDurationBuckets follows the current OpenTelemetry advisory layout shared by
// the HTTP and RPC signals (docs/metrics-contract.md). Update only in lockstep
// with the spec.
var rpcDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075,
	0.1, 0.25, 0.5, 0.75,
	1, 2.5, 5, 7.5, 10,
}

// newRPCDurationHistogram builds a float64 seconds histogram for the given RPC
// duration metric name on the provided meter. Returns nil if the meter is nil
// or instrument creation fails - callers must treat nil as "do not record".
func newRPCDurationHistogram(meter metric.Meter, name, description string) metric.Float64Histogram {
	if meter == nil {
		return nil
	}

	hist, err := meter.Float64Histogram(
		name,
		metric.WithUnit("s"),
		metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(rpcDurationBuckets...),
	)
	if err != nil {
		return nil
	}

	return hist
}

// classifyGRPCErrorType returns the low-cardinality error.type label for the
// RPC duration metrics. Any non-OK gRPC status maps to the canonical code name
// (a bounded enum, e.g. "NotFound", "Unavailable"), and OK maps to "" so
// successful calls carry no error.type. Using the code name rather than the
// handler's Go error type keeps the label set bounded regardless of how many
// distinct application errors flow through.
func classifyGRPCErrorType(code grpccodes.Code) string {
	if code == grpccodes.OK {
		return ""
	}

	return code.String()
}

// NormalizeGRPCError proves the error value returned by a gRPC
// handler or invoker is safe to stringify, rather than inspecting its shape.
//
// google.golang.org/grpc/status.FromError - and therefore status.Code, called
// throughout this file for telemetry - calls err.Error() unconditionally on
// its fallback path for any error that does not already carry a grpc Status.
// A shape check like log.IsNil alone only catches a bare top-level typed-nil
// (var err *MyError; return nil, err); it misses a VALID, non-nil error whose
// Unwrap chain hits an unguarded typed-nil - errors.Join(real, typedNil), or
// a custom wrapper whose Error() blindly delegates to a nil field - both of
// which are also not caught by IsNil and both panic on Error(). Unlike an
// HTTP handler panic, which Fiber can isolate to one request/response, a
// panic here runs in the goroutine servicing the RPC with nothing above it to
// recover: unrecovered, it takes down the whole process. The same raw error
// is also what this interceptor returns to grpc-go's own dispatch, which
// performs the identical status.FromError conversion one level up - so the
// value must be normalized here, not only read defensively for our own
// telemetry.
func NormalizeGRPCError(err error) error {
	if err == nil {
		return nil
	}

	if !log.IsSafeToStringify(err) {
		return status.Error(grpccodes.Unknown, "internal error")
	}

	return err
}

type spanEndStateKey struct{}

type spanEndState struct {
	span trace.Span
	// once guarantees End() is idempotent. It still protects the foreign/handler
	// -created span on the gRPC fallback path (where owned==false and the span
	// may be ended both by a defer and by the End interceptor). owned resolves
	// ordering (which interceptor ends the span, and that it happens after
	// finalization); once resolves double-end.
	once sync.Once
	// owned marks the span as exclusively finalized/ended by the interceptor
	// that created it (WithTelemetryInterceptor). When set,
	// EndTracingSpansInterceptor must NOT end it: the owning interceptor ends it
	// via its own deferred End() AFTER applying rpc.method / status attributes.
	owned bool
}

func newSpanEndState(span trace.Span) *spanEndState {
	return &spanEndState{span: span}
}

func (s *spanEndState) End() {
	if s == nil || s.span == nil {
		return
	}

	s.once.Do(func() { s.span.End() })
}

func contextWithSpanEndState(ctx context.Context, state *spanEndState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, spanEndStateKey{}, state)
}

func spanEndStateFromContext(ctx context.Context) *spanEndState {
	if ctx == nil {
		return nil
	}

	state, _ := ctx.Value(spanEndStateKey{}).(*spanEndState)

	return state
}

// TelemetryMiddleware wraps gRPC handlers with tracing and metrics setup.
type TelemetryMiddleware struct {
	Telemetry *tracing.Telemetry
}

// NewTelemetryMiddleware creates a new instance of TelemetryMiddleware.
func NewTelemetryMiddleware(tl *tracing.Telemetry) *TelemetryMiddleware {
	return &TelemetryMiddleware{tl}
}

// collectMetrics ensures the background metrics collector goroutine is running.
// It delegates to telemetrycore so the gRPC interceptors and the HTTP
// middleware share a single collector singleton.
func (tm *TelemetryMiddleware) collectMetrics(_ context.Context) error {
	if tm == nil {
		return nil
	}

	return telemetrycore.EnsureMetricsCollector(tm.Telemetry)
}

// WithTelemetryInterceptor is a gRPC interceptor that adds tracing to the context.
//
// When the effective Telemetry has a non-nil MeterProvider AND a non-nil
// MetricsFactory, the interceptor also records the rpc.server.duration
// (Float64 seconds) histogram for every call, independently of whether tracing
// is enabled - mirroring the HTTP WithTelemetry gate. Recording is best-effort:
// nil telemetry / MeterProvider / MetricsFactory and instrument-creation errors
// all silently skip the metric without affecting the request path.
func (tm *TelemetryMiddleware) WithTelemetryInterceptor(tl *tracing.Telemetry) grpc.UnaryServerInterceptor {
	// Build the server duration histogram once at interceptor-construction time,
	// symmetric to the HTTP WithTelemetry construction-once block.
	var serverDurationHistogram metric.Float64Histogram

	bootstrapTelemetry := tl
	if bootstrapTelemetry == nil && tm != nil {
		bootstrapTelemetry = tm.Telemetry
	}

	if bootstrapTelemetry != nil &&
		bootstrapTelemetry.MeterProvider != nil &&
		bootstrapTelemetry.MetricsFactory != nil {
		serverDurationHistogram = newRPCDurationHistogram(
			bootstrapTelemetry.MeterProvider.Meter(bootstrapTelemetry.LibraryName),
			rpcServerDurationMetric,
			"Duration of gRPC server calls.",
		)
	}

	// Same hoisting rationale as the histogram above: read once at
	// construction time rather than on every request. Shared with HTTP's
	// WithTelemetry - see TrustInboundTraceContext's doc comment - so a
	// service configures inbound trust once per Telemetry instance,
	// consistently across both transports.
	trustInboundTraceContext := bootstrapTelemetry != nil && bootstrapTelemetry.TrustInboundTraceContext

	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		ctx = normalizeGRPCContext(ctx)

		effectiveTelemetry := tl
		if effectiveTelemetry == nil && tm != nil {
			effectiveTelemetry = tm.Telemetry
		}

		if effectiveTelemetry == nil {
			return handler(ctx, req)
		}

		requestID := resolveGRPCRequestID(ctx, req)
		ctx = observability.ContextWithHeaderID(ctx, requestID)

		methodName := "unknown"
		if info != nil {
			methodName = info.FullMethod
		}

		if effectiveTelemetry.TracerProvider == nil {
			start := time.Now()
			resp, err := handler(ctx, req)
			err = NormalizeGRPCError(err)

			recordRPCDuration(ctx, serverDurationHistogram, methodName, start, err,
				ResolveTenantIDFromGRPC(ctx))

			return resp, err
		}

		tracer := effectiveTelemetry.TracerProvider.Tracer(effectiveTelemetry.LibraryName)

		tenantID := ResolveTenantIDFromGRPC(ctx)
		if tenantID != "" {
			ctx = observability.ContextWithSpanAttributes(ctx, attribute.String(constant.AttrKeyTenantID, tenantID))
		}

		ctx = observability.ContextWithSpanAttributes(ctx,
			attribute.String("app.request.request_id", requestID),
			attribute.String("grpc.method", methodName),
		)

		// Fail-closed by default (TrustInboundTraceContext's zero value),
		// same posture and same flag as the HTTP middleware: an untrusted
		// caller cannot choose this service's trace ID or force a sampling
		// decision via a forged traceparent. The previous gate - the
		// caller's User-Agent matching an internal-Lerian-service pattern -
		// was an interoperability hint, not an authenticated trust boundary
		// (spoofable by any caller that sets the header), so it never
		// actually restricted anything a malicious caller couldn't bypass.
		traceCtx := ctx
		if trustInboundTraceContext {
			md, _ := metadata.FromIncomingContext(ctx)
			traceCtx = tracing.ExtractGRPCContext(ctx, md)
		}

		ctx, span := tracer.Start(traceCtx, methodName, trace.WithSpanKind(trace.SpanKindServer))
		endState := newSpanEndState(span)
		// WithTelemetryInterceptor owns this span's lifecycle: it applies
		// rpc.method / rpc.grpc.status_code / handler error status AFTER the
		// handler returns (below), then ends the span via the defer. Marking it
		// owned makes EndTracingSpansInterceptor skip it, so those post-handler
		// attributes can't be dropped by a chain where the end interceptor
		// unwinds first — mirroring the HTTP WithTelemetry/EndTracingSpans pair.
		endState.owned = true

		defer endState.End()

		ctx = observability.ContextWithTracer(ctx, tracer)
		ctx = observability.ContextWithMetricFactory(ctx, effectiveTelemetry.MetricsFactory)
		ctx = contextWithSpanEndState(ctx, endState)

		err := tm.collectMetrics(ctx)
		if err != nil {
			tracing.HandleSpanError(span, "Failed to collect metrics", err)
		}

		// Capture start immediately before the handler so the duration metric
		// reflects the handler chain, then record after the status is known.
		start := time.Now()
		resp, err := handler(ctx, req)
		// Guard against a handler returning an error status.Code below (via
		// status.FromError) cannot safely stringify: err != nil is true even
		// for a bare typed-nil (var err *MyError; return nil, err), and
		// status.FromError calls err.Error() unconditionally on its fallback
		// path for anything that is not already a grpc Status - a
		// nil-receiver method call that panics the serving goroutine and,
		// unrecovered, kills the whole process. NormalizeGRPCError
		// proves stringifiability directly (see its own doc comment); it is
		// NOT limited to the top-level value the way
		// middleware.normalizeHTTPHandlerError is.
		err = NormalizeGRPCError(err)

		grpcStatusCode := status.Code(err)
		span.SetAttributes(
			attribute.String("rpc.method", methodName),
			attribute.Int("rpc.grpc.status_code", int(grpcStatusCode)),
		)

		if err != nil {
			tracing.HandleSpanError(span, "gRPC handler error", err)
		}

		recordRPCDuration(ctx, serverDurationHistogram, methodName, start, err, tenantID)

		return resp, err
	}
}

// recordRPCDuration emits an RPC duration histogram observation (server or
// client) for a completed unary call. It is a no-op when the histogram is nil
// (telemetry / MeterProvider / MetricsFactory absent or instrument creation
// failed), so callers can invoke it unconditionally.
//
// Attribute set follows the metric contract (docs/metrics-contract.md):
//   - rpc.system: always "grpc"
//   - rpc.method: the gRPC full method (bounded set of registered methods)
//   - rpc.grpc.status_code: the numeric gRPC status code, matching the span
//     attribute this library already emits
//   - error.type: only set for non-OK statuses, using the canonical code name
//     (a bounded enum) to keep cardinality low
//   - tenant.id: server-side only, passed by the caller (already resolved via
//     ResolveTenantIDFromGRPC); the client path passes "" so the label is
//     omitted, since a client does not own the tenant boundary
func recordRPCDuration(
	ctx context.Context,
	hist metric.Float64Histogram,
	methodName string,
	start time.Time,
	callErr error,
	tenantID string,
) {
	if hist == nil {
		return
	}

	grpcStatusCode := status.Code(callErr)

	attrs := []attribute.KeyValue{
		attribute.String("rpc.system", rpcSystemGRPC),
		attribute.String("rpc.method", methodName),
		attribute.Int("rpc.grpc.status_code", int(grpcStatusCode)),
	}

	if errType := classifyGRPCErrorType(grpcStatusCode); errType != "" {
		attrs = append(attrs, attribute.String("error.type", errType))
	}

	if tenantID != "" {
		attrs = append(attrs, attribute.String(constant.AttrKeyTenantID, tenantID))
	}

	hist.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

// EndTracingSpansInterceptor is a gRPC interceptor that ends the tracing spans.
func (tm *TelemetryMiddleware) EndTracingSpansInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		resp, err := handler(ctx, req)
		if state := spanEndStateFromContext(ctx); state != nil {
			// A span owned by WithTelemetryInterceptor is finalized and ended by
			// that interceptor after it records post-handler attributes/status.
			// Skip it here (same reasoning as the HTTP EndTracingSpans).
			if state.owned {
				return resp, err
			}

			state.End()

			return resp, err
		}

		trace.SpanFromContext(ctx).End()

		return resp, err
	}
}

// UnaryClientInterceptor is a gRPC client interceptor that propagates trace
// context on outgoing calls and records the rpc.client.duration (Float64
// seconds) histogram.
//
// The histogram is built once at construction time and gated on a non-nil
// MeterProvider AND MetricsFactory, mirroring the server interceptor. Trace
// context is injected into the outgoing metadata via tracing.InjectGRPCContext
// so downstream services join the trace instead of starting a new root.
// Recording and injection are best-effort: nil telemetry degrades to a plain
// pass-through to the invoker, never blocking the call.
//
// The client metric intentionally OMITS tenant.id: a client does not own the
// tenant boundary, and the server side already attributes the call to a tenant.
func (tm *TelemetryMiddleware) UnaryClientInterceptor(tl *tracing.Telemetry) grpc.UnaryClientInterceptor {
	var clientDurationHistogram metric.Float64Histogram

	bootstrapTelemetry := tl
	if bootstrapTelemetry == nil && tm != nil {
		bootstrapTelemetry = tm.Telemetry
	}

	if bootstrapTelemetry != nil &&
		bootstrapTelemetry.MeterProvider != nil &&
		bootstrapTelemetry.MetricsFactory != nil {
		clientDurationHistogram = newRPCDurationHistogram(
			bootstrapTelemetry.MeterProvider.Meter(bootstrapTelemetry.LibraryName),
			rpcClientDurationMetric,
			"Duration of gRPC client calls.",
		)
	}

	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx = normalizeGRPCContext(ctx)

		// Inject trace context into the outgoing metadata so the downstream
		// server joins this trace. Merge with any metadata already on the
		// outgoing context rather than overwriting it.
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok || md == nil {
			md = metadata.New(nil)
		} else {
			md = md.Copy()
		}

		md = tracing.InjectGRPCContext(ctx, md)
		ctx = metadata.NewOutgoingContext(ctx, md)

		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		err = NormalizeGRPCError(err)

		// Client metric carries no tenant.id (empty string omits the label).
		recordRPCDuration(ctx, clientDurationHistogram, method, start, err, "")

		return err
	}
}

// resolveGRPCRequestID determines the request ID for a gRPC call from the request body,
// existing context header, gRPC metadata (in that priority order), or generates a new UUID.
func resolveGRPCRequestID(ctx context.Context, req any) string {
	if rid, ok := getValidBodyRequestID(req); ok {
		return rid
	}

	if existing := getContextHeaderID(ctx); existing != "" {
		return existing
	}

	if rid := getMetadataID(ctx); rid != "" {
		return rid
	}

	return uuid.New().String()
}

// getContextHeaderID extracts the HeaderID from the observability context value.
func getContextHeaderID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	cv, ok := ctx.Value(observability.ContextKey).(*observability.ContextValue)
	if !ok || cv == nil {
		return ""
	}

	return normalizeRequestID(cv.HeaderID)
}

// getValidBodyRequestID extracts and validates the request_id from the gRPC request body.
// Returns (id, true) when present and valid UUID; otherwise ("", false).
func getValidBodyRequestID(req any) (string, bool) {
	if req == nil {
		return "", false
	}

	// Check for typed-nil interface.
	v := reflect.ValueOf(req)
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
		return "", false
	}

	r, ok := req.(interface{ GetRequestId() string })
	if !ok {
		return "", false
	}

	rid := strings.TrimSpace(r.GetRequestId())
	if rid == "" {
		return "", false
	}

	// Validate it is a UUID.
	if _, err := uuid.Parse(rid); err != nil {
		return "", false
	}

	return rid, true
}

// normalizeRequestID trims whitespace and control characters from a raw request ID.
func normalizeRequestID(raw string) string {
	return strings.TrimSpace(sanitizeLogValue(raw))
}

// sanitizeLogValue strips ASCII control bytes (log-injection defense) from raw.
func sanitizeLogValue(raw string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}

		return r
	}, raw)
}

// isNilOrEmptyString reports whether a string pointer is nil or the trimmed
// value is empty. "null" and "nil" are treated as empty to handle JSON null
// serialization artifacts where some encoders emit the literal string "null"
// or "nil" instead of a JSON null.
func isNilOrEmptyString(s *string) bool {
	return s == nil || strings.TrimSpace(*s) == "" || strings.TrimSpace(*s) == "null" || strings.TrimSpace(*s) == "nil"
}

// normalizeGRPCContext returns a non-nil context, falling back to context.Background().
func normalizeGRPCContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}

	return ctx
}

// getMetadataID extracts a correlation id from incoming gRPC metadata if present.
func getMetadataID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	if md, ok := metadata.FromIncomingContext(ctx); ok && md != nil {
		headerID := md.Get(metadataID)
		if len(headerID) > 0 {
			v := normalizeRequestID(headerID[0])
			if v != "" && v != "null" && v != "nil" {
				return v
			}
		}
	}

	return ""
}

// ResolveTenantIDFromGRPC returns the tenant identifier carried by the
// canonical tenant-id gRPC metadata key, normalized for safe inclusion in
// telemetry. Returns an empty string when the metadata is absent, empty, or
// longer than MaxTenantIDLen bytes. The metadata is trusted only as an
// observability hint: callers MUST authenticate the tenant separately.
//
// Known gap (tracked, not yet closed): unlike the tenant.id OTel baggage
// member - which tracing.ExtractTraceContext strips unconditionally from
// every inbound carrier, see its doc comment - this function reads a
// caller-controlled `tenant-id` gRPC metadata field DIRECTLY, with no
// equivalent strip, and its result is stamped onto spans and the
// rpc.server.duration metric's tenant.id label. The gap is twofold:
// attribution forgery (a caller can claim any tenant), and metric label
// cardinality - sanitizeTenantID caps each value's LENGTH but not the number
// of DISTINCT values, so a caller sending unlimited distinct tenant-id
// values creates unbounded rpc.server.duration series, growing memory in the
// SDK's aggregation store and in the metrics backend (the span attribute is
// bounded per trace and unaffected; the metric label is the exposed
// surface). Whether to close this the same way (and what that breaks for
// callers that already rely on it as a deliberate cross-service hint) is a
// separate, pending product decision covering both risks - not addressed by
// this fix.
func ResolveTenantIDFromGRPC(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || md == nil {
		return ""
	}

	vals := md.Get(constant.MetadataTenantID)
	if len(vals) == 0 {
		return ""
	}

	return sanitizeTenantID(vals[0])
}

// sanitizeTenantID trims whitespace and control bytes from raw, then enforces
// the MaxTenantIDLen cap. Returns "" for any value that fails normalization or
// exceeds the cap, so callers can use it as a presence check.
func sanitizeTenantID(raw string) string {
	value := normalizeRequestID(raw)
	if isNilOrEmptyString(&value) {
		return ""
	}

	if len(value) > constant.MaxTenantIDLen {
		return ""
	}

	return value
}
