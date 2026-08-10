// Package middleware provides Fiber HTTP telemetry middleware that integrates
// with the lib-observability tracing and metrics packages.
//
// The gRPC telemetry interceptors live in the sibling grpcmiddleware package,
// which is Fiber-free so Fiber-v2 applications can consume them without pulling
// in Fiber v3. Both packages share the single process-wide system-metrics
// collector via telemetrycore.
package middleware

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	observability "github.com/LerianStudio/lib-observability/v2"
	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// httpServerRequestDurationMetric is the OpenTelemetry semantic-convention metric name
// for HTTP server request duration. Recorded as a Float64 histogram in seconds.
const httpServerRequestDurationMetric = "http.server.request.duration"

// httpServerActiveRequestsMetric is the OpenTelemetry semantic-convention metric
// name for the number of in-flight HTTP server requests. Recorded as an Int64
// UpDownCounter in the unitless "{request}" dimension.
const httpServerActiveRequestsMetric = "http.server.active_requests"

const httpHandlerErrorEvent = "http.handler.error"

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

// newActiveRequestsCounter builds the int64 UpDownCounter instrument for
// http.server.active_requests on the given meter. Returns nil if the meter is
// nil or instrument creation fails - callers must treat nil as "do not record".
func newActiveRequestsCounter(meter metric.Meter) metric.Int64UpDownCounter {
	if meter == nil {
		return nil
	}

	counter, err := meter.Int64UpDownCounter(
		httpServerActiveRequestsMetric,
		metric.WithUnit("{request}"),
		metric.WithDescription("Number of active HTTP server requests."),
	)
	if err != nil {
		return nil
	}

	return counter
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

// TelemetryMiddleware wraps Fiber HTTP handlers with tracing and metrics setup.
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
	var (
		durationHistogram metric.Float64Histogram
		activeRequests    metric.Int64UpDownCounter
	)

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
		meter := bootstrapTelemetry.MeterProvider.Meter(bootstrapTelemetry.LibraryName)
		durationHistogram = newHTTPServerDurationHistogram(meter)
		activeRequests = newActiveRequestsCounter(meter)
	}

	// Same hoisting rationale as the histogram above: read once at
	// construction time rather than on every request.
	trustInboundTraceContext := bootstrapTelemetry != nil && bootstrapTelemetry.TrustInboundTraceContext

	return func(c fiber.Ctx) error {
		effectiveTelemetry := tl
		if effectiveTelemetry == nil && tm != nil {
			effectiveTelemetry = tm.Telemetry
		}

		if effectiveTelemetry == nil {
			return nextWithNormalizedHTTPError(c)
		}

		if len(excludedRoutes) > 0 && isRouteExcludedFromList(c, excludedRoutes) {
			return nextWithNormalizedHTTPError(c)
		}

		setRequestHeaderID(c)

		// Capture the request start time before any downstream work so the
		// duration metric reflects the full handler chain, regardless of
		// whether tracing is enabled below.
		requestStart := time.Now()

		ctx := c.Context()
		_, _, reqId, _ := observability.NewTrackingFromContext(ctx)

		c.SetContext(observability.ContextWithSpanAttributes(ctx,
			attribute.String("app.request.request_id", reqId),
		))

		// Capture all Fiber context string values BEFORE c.Next(). Fiber uses
		// utils.UnsafeString which returns pointers into fasthttp's request buffer.
		// After c.Next() returns, fasthttp may recycle the underlying RequestCtx
		// for the next connection, corrupting any previously returned string slices.
		// Safe copies via string([]byte(...)) ensure the data is heap-owned.
		rawMethod := string([]byte(c.Method()))
		method, methodOriginal, methodReplaced := normalizeHTTPMethod(rawMethod)

		// Track in-flight requests around the full downstream chain. Increment
		// before c.Next() and decrement on return via the returned closure, so
		// the counter reflects concurrency across both the tracing and
		// no-tracer paths below. No-op when the instrument is nil.
		activeDone := trackActiveRequest(c.Context(), activeRequests, method)
		defer activeDone()

		if effectiveTelemetry.TracerProvider == nil {
			returnedErr := c.Next()
			chainErr, _, statusCode := resolveHTTPResponse(c, returnedErr)

			recordHTTPServerDuration(c, durationHistogram, method, requestStart, statusCode)

			return chainErr
		}

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

		// Fail-closed by default (TrustInboundTraceContext's zero value):
		// inbound trace context is only extracted for a deployment that opted
		// in, so an untrusted caller cannot choose this service's trace ID or
		// force a sampling decision via a forged traceparent header. When
		// extraction does run, ExtractHTTPContext always strips the tenant.id
		// baggage member regardless of this flag - tenant identity never comes
		// from a header.
		traceCtx := c.Context()
		if trustInboundTraceContext {
			traceCtx = ExtractHTTPContext(traceCtx, c)
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

		returnedErr := c.Next()
		chainErr, handlerErr, statusCode := resolveHTTPResponse(c, returnedErr)

		applyTelemetrySpanAttributes(span, c, statusCode, telemetryRequestAttrs{
			method:         method,
			methodOriginal: methodOriginal,
			methodReplaced: methodReplaced,
			protocol:       protocol,
			hostname:       hostname,
			userAgent:      userAgent,
			handlerErr:     handlerErr,
		})

		recordHTTPServerDuration(c, durationHistogram, method, requestStart, statusCode)

		return chainErr
	}
}

// telemetryRequestAttrs groups the per-request fields needed to apply OTel
// span attributes after c.Next() returns. Kept package-private; only
// applyTelemetrySpanAttributes consumes it.
type telemetryRequestAttrs struct {
	method         string
	methodOriginal string
	methodReplaced bool
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
	resolvedRoute := resolvedHTTPRoute(c)

	spanAttrs := []attribute.KeyValue{
		attribute.String("http.request.method", req.method),
		attribute.String("url.path", resolvedRoute),
		attribute.String("url.scheme", req.protocol),
		attribute.String("server.address", req.hostname),
		attribute.String("user_agent.original", truncateUserAgent(req.userAgent)),
		attribute.Int("http.response.status_code", statusCode),
	}
	if routePath, present := routeAttribute(c); present {
		spanAttrs = append(spanAttrs, attribute.String("http.route", routePath))
	}

	// Rename only after routing has resolved. Matched traffic uses the route
	// template; unmatched traffic uses one stable fallback. Neither path can
	// retain concrete identifiers or query values.
	if span.IsRecording() {
		span.SetName(req.method + " " + resolvedRoute)
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

	// The error-recording mechanism is status-driven, status itself stays
	// gated on statusCode below regardless of which branch runs, so a 4xx
	// handler error never flips the span to Error per OTel semconv:
	//   - >=500: span.RecordError produces the OTel semconv "exception" event
	//     (exception.type/exception.message), which APM backends (Tempo,
	//     Jaeger) index on - a custom-named event does not surface there.
	//   - <500: the custom http.handler.error event (unaffected by this
	//     split; kept for handler errors that don't warrant "exception"
	//     status, e.g. a mapped 4xx).
	// tracing.ErrorMessage is unconditionally safe to call - it routes
	// through log.SafeErrorMessage, which never panics regardless of
	// req.handlerErr's shape (nil, typed-nil, or a valid-but-unsafe-to
	// -stringify error).
	if req.handlerErr != nil {
		if statusCode >= 500 {
			span.RecordError(errors.New(tracing.ErrorMessage(req.handlerErr)))
		} else {
			tracing.HandleSpanBusinessErrorEvent(span, httpHandlerErrorEvent, req.handlerErr)
		}
	}

	if statusCode >= 500 {
		span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(statusCode))
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
//
// Tenant and customer identity are deliberately excluded. Request duration
// histograms are infrastructure telemetry and may only use stable transport
// dimensions; identity would create an unbounded series per customer.
func recordHTTPServerDuration(
	c fiber.Ctx,
	hist metric.Float64Histogram,
	method string,
	start time.Time,
	statusCode int,
) {
	if hist == nil || c == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.Int("http.response.status_code", statusCode),
	}
	if routePath, present := routeAttribute(c); present {
		attrs = append(attrs, attribute.String("http.route", routePath))
	}

	if errType := classifyHTTPErrorType(statusCode); errType != "" {
		attrs = append(attrs, attribute.String("error.type", errType))
	}

	durationSeconds := time.Since(start).Seconds()
	hist.Record(c.Context(), durationSeconds, metric.WithAttributes(attrs...))
}

// trackActiveRequest increments the http.server.active_requests UpDownCounter by
// one and returns a closure that decrements it by one when invoked (deferred by
// the caller). The label set is intentionally minimal - only
// http.request.method - to keep the concurrency gauge low-cardinality;
// http.route is deliberately omitted because it is not reliably known before
// routing (c.Next), and adding it would multiply the series without adding
// operational value for an in-flight gauge. It is a no-op (returns a no-op
// closure) when the counter is nil, so callers can invoke it unconditionally.
func trackActiveRequest(ctx context.Context, counter metric.Int64UpDownCounter, method string) func() {
	if counter == nil {
		return func() {}
	}

	attrs := metric.WithAttributes(attribute.String("http.request.method", method))
	counter.Add(ctx, 1, attrs)

	return func() {
		counter.Add(ctx, -1, attrs)
	}
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
	err := normalizeHTTPHandlerError(c.Next())

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
