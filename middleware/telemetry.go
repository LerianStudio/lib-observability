// Package middleware provides Fiber HTTP and gRPC telemetry middleware that
// integrates with the lib-observability tracing and metrics packages.
package middleware

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	observability "github.com/LerianStudio/lib-observability/v2"
	constant "github.com/LerianStudio/lib-observability/v2/constants"
	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// httpServerRequestDurationMetric is the OpenTelemetry semantic-convention metric name
// for HTTP server request duration. Recorded as a Float64 histogram in seconds.
const httpServerRequestDurationMetric = "http.server.request.duration"

// httpServerDurationBuckets follows the current OpenTelemetry HTTP semantic
// conventions advisory layout for http.server.request.duration. Update only
// in lockstep with the spec.
var httpServerDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075,
	0.1, 0.25, 0.5, 0.75,
	1, 2.5, 5, 7.5, 10,
}

// newHTTPServerDurationHistogram builds the float64 histogram instrument for
// http.server.request.duration on the given meter. Returns nil if the meter is
// nil or instrument creation fails - callers must treat nil as "do not record".
func newHTTPServerDurationHistogram(meter metric.Meter) metric.Float64Histogram {
	if meter == nil {
		return nil
	}

	hist, err := meter.Float64Histogram(
		httpServerRequestDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests."),
		metric.WithExplicitBucketBoundaries(httpServerDurationBuckets...),
	)
	if err != nil {
		return nil
	}

	return hist
}

// Header and metadata key constants used by the middleware.
const (
	// headerID is the request identifier header key.
	headerID = "X-Request-Id"
	// headerUserAgent is the HTTP User-Agent header key.
	headerUserAgent = "User-Agent"
	// metadataID is the gRPC metadata key that carries the request context identifier.
	metadataID = "metadata_id"
)

// ErrContextNotFound is returned when a required Fiber context is nil.
var ErrContextNotFound = errors.New("fiber context not found")

type spanEndStateKey struct{}

type spanEndState struct {
	span trace.Span
	// once guarantees End() is idempotent. It is retained (not superseded by
	// owned) because it still protects the foreign/handler-created span on the
	// HTTP and gRPC fallback paths (where owned==false and the span may be
	// ended both by a defer and by the End middleware). owned resolves ordering
	// (which middleware ends the span, and that it happens after finalization);
	// once resolves double-end.
	once sync.Once
	// owned marks the span as exclusively finalized/ended by the middleware
	// that created it (WithTelemetry). When set, EndTracingSpans must NOT end
	// it: the owning middleware ends it via its own deferred End() AFTER
	// applying the route template name and post-c.Next() attributes. This
	// removes the ordering hazard where a separately-registered EndTracingSpans
	// unwinds first (Fiber LIFO) and ends the span before finalization,
	// silently discarding http.route/status/error.type and the span rename.
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

// TelemetryMiddleware wraps HTTP and gRPC handlers with tracing and metrics setup.
type TelemetryMiddleware struct {
	Telemetry *tracing.Telemetry
}

// NewTelemetryMiddleware creates a new instance of TelemetryMiddleware.
func NewTelemetryMiddleware(tl *tracing.Telemetry) *TelemetryMiddleware {
	return &TelemetryMiddleware{tl}
}

// WithTelemetry is a middleware that adds tracing to the context.
//
// When the effective Telemetry has a non-nil MeterProvider AND a non-nil
// MetricsFactory, the middleware also records the OpenTelemetry semantic-
// convention HTTP server metric `http.server.request.duration` (Float64 seconds
// histogram) for every non-excluded request. Recording is best-effort: nil
// telemetry, nil MeterProvider, nil MetricsFactory, excluded routes, and
// instrument creation errors all silently skip the metric without affecting
// the request path or existing span behavior.
func (tm *TelemetryMiddleware) WithTelemetry(tl *tracing.Telemetry, excludedRoutes ...string) fiber.Handler {
	// Build the duration histogram once at handler-construction time. The
	// effective Telemetry may be supplied either via the explicit `tl` argument
	// or via the receiver's stored Telemetry, mirroring the per-request logic
	// below. If neither resolves, or any required component is nil, the
	// histogram is left nil and recording is skipped.
	var durationHistogram metric.Float64Histogram

	bootstrapTelemetry := tl
	if bootstrapTelemetry == nil && tm != nil {
		bootstrapTelemetry = tm.Telemetry
	}

	// MetricsFactory presence is used here as the canonical "metrics subsystem
	// enabled" signal across this library, even though the histogram itself is
	// built directly from MeterProvider below. Keeping this gate aligned with
	// the rest of the metrics package (see metrics/doc.go) ensures callers that
	// disable metrics by nil-ing MetricsFactory also stop receiving the duration
	// histogram, without us needing a separate enablement flag.
	if bootstrapTelemetry != nil &&
		bootstrapTelemetry.MeterProvider != nil &&
		bootstrapTelemetry.MetricsFactory != nil {
		durationHistogram = newHTTPServerDurationHistogram(
			bootstrapTelemetry.MeterProvider.Meter(bootstrapTelemetry.LibraryName),
		)
	}

	return func(c fiber.Ctx) error {
		effectiveTelemetry := tl
		if effectiveTelemetry == nil && tm != nil {
			effectiveTelemetry = tm.Telemetry
		}

		if effectiveTelemetry == nil {
			return c.Next()
		}

		if len(excludedRoutes) > 0 && isRouteExcludedFromList(c, excludedRoutes) {
			return c.Next()
		}

		setRequestHeaderID(c)

		// Capture the request start time before any downstream work so the
		// duration metric reflects the full handler chain, regardless of
		// whether tracing is enabled below.
		requestStart := time.Now()

		ctx := c.Context()
		_, _, reqId, _ := observability.NewTrackingFromContext(ctx)

		if tenantID := ResolveTenantIDFromHTTP(c); tenantID != "" {
			ctx = observability.ContextWithSpanAttributes(ctx, attribute.String(constant.AttrKeyTenantID, tenantID))
		}

		c.SetContext(observability.ContextWithSpanAttributes(ctx,
			attribute.String("app.request.request_id", reqId),
		))

		// Capture all Fiber context string values BEFORE c.Next(). Fiber v2 uses
		// utils.UnsafeString which returns pointers into fasthttp's request buffer.
		// After c.Next() returns, fasthttp may recycle the underlying RequestCtx
		// for the next connection, corrupting any previously returned string slices.
		// Safe copies via string([]byte(...)) ensure the data is heap-owned.
		rawMethod := string([]byte(c.Method()))
		method, methodOriginal, methodReplaced := normalizeHTTPMethod(rawMethod)

		if effectiveTelemetry.TracerProvider == nil {
			err := c.Next()

			recordHTTPServerDuration(c, durationHistogram, method, requestStart, err)

			return err
		}

		originalURL := string([]byte(c.OriginalURL()))
		protocol := string([]byte(c.Protocol()))
		hostname := string([]byte(c.Hostname()))
		userAgent := string([]byte(c.Get(headerUserAgent)))

		tracer := effectiveTelemetry.TracerProvider.Tracer(effectiveTelemetry.LibraryName)
		// Create the span with a method-only name (e.g. "GET"). The route
		// template is not reliably known until after routing (c.Next), and the
		// concrete path carries PII / unbounded cardinality (IDs, Pix keys). A
		// method-only creation name keeps PII out of the name the sampler sees
		// and out of spans that are dropped or never match a route. After
		// c.Next, applyTelemetrySpanAttributes renames the span to
		// "{method} {route template}" once the route is known.
		spanName := method

		traceCtx := c.Context()
		// Compatibility note: trace extraction currently trusts the internal-service
		// User-Agent heuristic. This is an interoperability hint, not an authenticated
		// trust boundary, and is preserved to avoid changing existing caller behavior.
		if isInternalLerianService(userAgent) {
			traceCtx = tracing.ExtractHTTPContext(traceCtx, c)
		}

		ctx, span := tracer.Start(traceCtx, spanName, trace.WithSpanKind(trace.SpanKindServer))
		endState := newSpanEndState(span)
		// WithTelemetry owns this span's lifecycle: it is the sole ender (via the
		// defer below, which fires on return AFTER applyTelemetrySpanAttributes
		// has renamed and finalized the span). EndTracingSpans, if also
		// registered, detects the owned flag and does NOT end it.
		endState.owned = true

		defer endState.End()

		ctx = observability.ContextWithTracer(ctx, tracer)
		ctx = observability.ContextWithMetricFactory(ctx, effectiveTelemetry.MetricsFactory)
		ctx = contextWithSpanEndState(ctx, endState)
		c.SetContext(ctx)

		err := tm.collectMetrics(ctx)
		if err != nil {
			tracing.HandleSpanError(span, "Failed to collect metrics", err)
		}

		err = c.Next()

		// Reconcile the effective status the client will observe (same helper
		// the metric uses) so the span's status code, error.type, and
		// error.type_original stay consistent with the duration metric.
		statusCode := httpStatusCode(c, err)

		applyTelemetrySpanAttributes(span, c, statusCode, telemetryRequestAttrs{
			method:         method,
			methodOriginal: methodOriginal,
			methodReplaced: methodReplaced,
			originalURL:    originalURL,
			protocol:       protocol,
			hostname:       hostname,
			userAgent:      userAgent,
			handlerErr:     err,
		})

		recordHTTPServerDuration(c, durationHistogram, method, requestStart, err)

		return err
	}
}

// telemetryRequestAttrs groups the per-request fields needed to apply OTel
// span attributes after c.Next() returns. Kept package-private; only
// applyTelemetrySpanAttributes consumes it.
type telemetryRequestAttrs struct {
	method         string
	methodOriginal string
	methodReplaced bool
	originalURL    string
	protocol       string
	hostname       string
	userAgent      string
	handlerErr     error
}

// applyTelemetrySpanAttributes sets the OTel HTTP semantic-convention
// attributes on the request span and finalizes its status. Extracted from
// WithTelemetry to keep that function's cyclomatic complexity within the
// repo's lint budget; the behavior is identical to setting the attributes
// inline.
func applyTelemetrySpanAttributes(
	span trace.Span,
	c fiber.Ctx,
	statusCode int,
	req telemetryRequestAttrs,
) {
	spanAttrs := []attribute.KeyValue{
		attribute.String("http.request.method", req.method),
		attribute.String("url.path", sanitizeURL(req.originalURL)),
		attribute.String("url.scheme", req.protocol),
		attribute.String("server.address", req.hostname),
		attribute.String("user_agent.original", truncateUserAgent(req.userAgent)),
		attribute.Int("http.response.status_code", statusCode),
	}
	if routePath, present := routeAttribute(c, statusCode); present {
		spanAttrs = append(spanAttrs, attribute.String("http.route", routePath))

		// Rename the span from its method-only creation name to the OTel
		// convention "{method} {http.route}" now that routing has resolved the
		// low-cardinality, PII-free route template. Guarded by IsRecording so
		// the intent ("skip if already ended/not recording") is explicit and
		// testable rather than relying on the post-End() no-op. When no route
		// matched (routeAttribute present=false, e.g. catch-all 404) the
		// method-only name is left intact and http.route is omitted.
		if span.IsRecording() {
			span.SetName(req.method + " " + routePath)
		}
	}

	if req.methodReplaced {
		spanAttrs = append(spanAttrs, attribute.String("http.request.method_original", req.methodOriginal))
	}

	if errType := classifyHTTPErrorType(statusCode); errType != "" {
		spanAttrs = append(spanAttrs, attribute.String("error.type", errType))
	}

	if origType := errorTypeOriginal(req.handlerErr); origType != "" {
		spanAttrs = append(spanAttrs, attribute.String("error.type_original", origType))
	}

	span.SetAttributes(spanAttrs...)

	if req.handlerErr != nil {
		tracing.HandleSpanError(span, "handler error", req.handlerErr)
		return
	}

	if statusCode >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
	}
}

// recordHTTPServerDuration emits the http.server.request.duration histogram
// observation for a completed Fiber request. It is a no-op when the histogram
// is nil (telemetry/MeterProvider/MetricsFactory absent or instrument creation
// failed) so callers can invoke it unconditionally without nil checks.
//
// Attribute set follows OpenTelemetry HTTP semantic conventions:
//   - http.request.method: captured before c.Next() to survive fasthttp recycling
//   - http.route: c.Route().Path - low-cardinality route template, never raw paths;
//     omitted entirely when no route matched (Fiber's catch-all 404), so scanner/
//     unmatched traffic does not pollute the root-route series.
//   - http.response.status_code: the effective status the client will observe;
//     derived from the handler error (*fiber.Error.Code, or 500 for generic
//     errors) when Fiber's error handler has not yet rewritten the response,
//     otherwise read directly from the response. This matches httpStatusCode
//     used by the logging middleware and avoids reporting 200 for failures.
//   - error.type: only set when effective status >= 500, using the numeric
//     status code as a stable, low-cardinality label.
//   - tenant.id: resolved via resolveTenantIDForTelemetry, the same
//     AttrBag→baggage precedence used by the logging middleware and span
//     processor. This covers both the local-hop X-Tenant-Id header (resolved
//     into the AttrBag) AND tenant.id propagated cross-service via OTel
//     baggage; reading the AttrBag alone previously dropped the baggage case,
//     emitting an empty tenant_id label for downstream traffic. Omitted when
//     neither source carries a value so series for non-tenant traffic do not
//     gain an empty label. Cardinality is bounded by the 128-byte tenant cap in
//     middleware/tenant.go, keeping the label safe for use in dashboards and
//     alerts that filter by tenant.
func recordHTTPServerDuration(
	c fiber.Ctx,
	hist metric.Float64Histogram,
	method string,
	start time.Time,
	handlerErr error,
) {
	if hist == nil || c == nil {
		return
	}

	statusCode := httpStatusCode(c, handlerErr)

	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.Int("http.response.status_code", statusCode),
	}
	if routePath, present := routeAttribute(c, statusCode); present {
		attrs = append(attrs, attribute.String("http.route", routePath))
	}

	if errType := classifyHTTPErrorType(statusCode); errType != "" {
		attrs = append(attrs, attribute.String("error.type", errType))
	}

	if tenantID := resolveTenantIDForTelemetry(c.Context()); tenantID != "" {
		attrs = append(attrs, attribute.String(constant.AttrKeyTenantID, tenantID))
	}

	durationSeconds := time.Since(start).Seconds()
	hist.Record(c.Context(), durationSeconds, metric.WithAttributes(attrs...))
}

// classifyHTTPErrorType returns the stable, low-cardinality error.type
// label for the http.server.request.duration metric per OpenTelemetry HTTP
// semantic conventions. Status-driven by design: a 503 surfaced via
// fiber.NewError(503) and a 503 surfaced via c.SendStatus(503) MUST produce
// the same time series so alert rules of the form error_type=~"5.."
// aggregate reliably. The originating Go type identity, when useful for
// debugging, is published separately on the span via errorTypeOriginal.
func classifyHTTPErrorType(statusCode int) string {
	if statusCode >= 500 {
		return strconv.Itoa(statusCode)
	}

	return ""
}

// EndTracingSpans is a middleware that ends the tracing spans.
func (tm *TelemetryMiddleware) EndTracingSpans(c fiber.Ctx) error {
	if c == nil {
		return ErrContextNotFound
	}

	originalCtx := c.Context()
	err := c.Next()

	endCtx := c.Context()
	if endCtx == nil {
		endCtx = originalCtx
	}

	if state := spanEndStateFromContext(endCtx); state != nil {
		// A span owned by WithTelemetry is finalized and ended by WithTelemetry
		// itself (after it applies the route template name and post-c.Next
		// attributes). Ending it here would be a no-op at best and, due to
		// Fiber's LIFO unwinding, could race ahead of that finalization and
		// discard the rename/attributes. Return early and let the owning
		// middleware end it.
		if state.owned {
			return err
		}

		state.End()

		return err
	}

	if endCtx != nil {
		trace.SpanFromContext(endCtx).End()
	}

	return err
}

// WithTelemetryInterceptor is a gRPC interceptor that adds tracing to the context.
func (tm *TelemetryMiddleware) WithTelemetryInterceptor(tl *tracing.Telemetry) grpc.UnaryServerInterceptor {
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

		if effectiveTelemetry.TracerProvider == nil {
			return handler(ctx, req)
		}

		tracer := effectiveTelemetry.TracerProvider.Tracer(effectiveTelemetry.LibraryName)

		methodName := "unknown"
		if info != nil {
			methodName = info.FullMethod
		}

		if tenantID := ResolveTenantIDFromGRPC(ctx); tenantID != "" {
			ctx = observability.ContextWithSpanAttributes(ctx, attribute.String(constant.AttrKeyTenantID, tenantID))
		}

		ctx = observability.ContextWithSpanAttributes(ctx,
			attribute.String("app.request.request_id", requestID),
			attribute.String("grpc.method", methodName),
		)

		traceCtx := ctx
		// Compatibility note: trace extraction currently trusts the internal-service
		// User-Agent heuristic. This is an interoperability hint, not an authenticated
		// trust boundary, and is preserved to avoid changing existing caller behavior.
		if isInternalLerianService(getGRPCUserAgent(ctx)) {
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

		resp, err := handler(ctx, req)

		grpcStatusCode := status.Code(err)
		span.SetAttributes(
			attribute.String("rpc.method", methodName),
			attribute.Int("rpc.grpc.status_code", int(grpcStatusCode)),
		)

		if err != nil {
			tracing.HandleSpanError(span, "gRPC handler error", err)
		}

		return resp, err
	}
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

// setRequestHeaderID ensures the Fiber request carries a unique correlation ID header.
// The effective ID is always echoed back on the response so that callers can
// correlate their request regardless of whether the ID was client-supplied or
// server-generated.
func setRequestHeaderID(c fiber.Ctx) {
	hid := normalizeRequestID(c.Get(headerID))

	if isNilOrEmptyString(&hid) {
		hid = uuid.New().String()
	}

	c.Request().Header.Set(headerID, hid)
	c.Set(headerID, hid)
	c.Response().Header.Set(headerID, hid)

	ctx := observability.ContextWithHeaderID(c.Context(), hid)
	c.SetContext(ctx)
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
