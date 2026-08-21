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
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	observability "github.com/LerianStudio/lib-observability/v3"
	constant "github.com/LerianStudio/lib-observability/v3/constants"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

// httpServerRequestDurationMetric is the OpenTelemetry semantic-convention metric name
// for HTTP server request duration. Recorded as a Float64 histogram in seconds.
const httpServerRequestDurationMetric = "http.server.request.duration"

// authenticatedTenantHTTPServerRequestsMetric is an opt-in per-tenant request
// counter. It deliberately carries only tenant and route: 50 tenants across 30
// routes produce 1,500 attribute sets, below the OTel SDK default limit of 2,000.
const authenticatedTenantHTTPServerRequestsMetric = "lerian.http.server.requests.by_tenant"

// authenticatedTenantHTTPServerResponses5xxMetric is an opt-in per-tenant
// counter for requests resulting in HTTP 5xx responses. Keeping failures in a
// separate instrument preserves route-level error rates without multiplying the
// request counter by status values.
const authenticatedTenantHTTPServerResponses5xxMetric = "lerian.http.server.responses_5xx.by_tenant"

// authenticatedTenantHTTPServerResponses4xxMetric is an opt-in per-tenant
// counter for requests resulting in HTTP 4xx responses. It deliberately carries
// only tenant and route; exact status-code diagnosis remains a trace-level
// question so the metric cannot multiply by every possible 4xx code.
const authenticatedTenantHTTPServerResponses4xxMetric = "lerian.http.server.responses_4xx.by_tenant"

// authenticatedTenantHTTPServerLatencyMetric is an opt-in per-tenant latency
// histogram. It deliberately omits http.route and http.request.method: each
// label multiplies series by bucket_count+2, and the OTel SDK drops tenant
// identity into an otel.metric.overflow bucket past 2000 attribute sets.
// Response statuses are normalized to a bounded class before recording.
// Per-route latency is a trace-level question.
const authenticatedTenantHTTPServerLatencyMetric = "lerian.http.server.latency.by_tenant"

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

// newAuthenticatedTenantHTTPRequestsCounter builds the opt-in per-tenant request
// counter. Returns nil if the meter is nil or instrument creation fails.
func newAuthenticatedTenantHTTPRequestsCounter(meter metric.Meter) metric.Int64Counter {
	if meter == nil {
		return nil
	}

	counter, err := meter.Int64Counter(
		authenticatedTenantHTTPServerRequestsMetric,
		metric.WithUnit("{request}"),
		metric.WithDescription("Count of HTTP server requests partitioned by authenticated tenant."),
	)
	if err != nil {
		return nil
	}

	return counter
}

// newAuthenticatedTenantHTTPResponses5xxCounter builds the opt-in per-tenant
// HTTP 5xx response counter. Returns nil if the meter is nil or instrument
// creation fails.
func newAuthenticatedTenantHTTPResponses5xxCounter(meter metric.Meter) metric.Int64Counter {
	if meter == nil {
		return nil
	}

	counter, err := meter.Int64Counter(
		authenticatedTenantHTTPServerResponses5xxMetric,
		metric.WithUnit("{request}"),
		metric.WithDescription("Count of HTTP server requests resulting in 5xx responses partitioned by authenticated tenant."),
	)
	if err != nil {
		return nil
	}

	return counter
}

// newAuthenticatedTenantHTTPResponses4xxCounter builds the opt-in per-tenant
// HTTP 4xx response counter. Returns nil if the meter is nil or instrument
// creation fails.
func newAuthenticatedTenantHTTPResponses4xxCounter(meter metric.Meter) metric.Int64Counter {
	if meter == nil {
		return nil
	}

	counter, err := meter.Int64Counter(
		authenticatedTenantHTTPServerResponses4xxMetric,
		metric.WithUnit("{request}"),
		metric.WithDescription("Count of HTTP server requests resulting in 4xx responses partitioned by authenticated tenant."),
	)
	if err != nil {
		return nil
	}

	return counter
}

// newAuthenticatedTenantHTTPLatencyHistogram builds the opt-in per-tenant latency
// histogram. Shares httpServerDurationBuckets with the standard metric so both
// are directly comparable. Returns nil if the meter is nil or creation fails.
func newAuthenticatedTenantHTTPLatencyHistogram(meter metric.Meter) metric.Float64Histogram {
	if meter == nil {
		return nil
	}

	hist, err := meter.Float64Histogram(
		authenticatedTenantHTTPServerLatencyMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests partitioned by authenticated tenant."),
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
	// silently discarding http.route/status/error.type and the span rename -
	// measured: the span ends up named "GET" instead of "GET /orders/:id".
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
	return &TelemetryMiddleware{Telemetry: tl}
}

// httpServerInstruments groups the HTTP server instruments built once at
// handler-construction time. A struct (rather than positional returns) makes it
// impossible to transpose the standard and per-tenant instruments.
type httpServerInstruments struct {
	duration       metric.Float64Histogram
	activeRequests metric.Int64UpDownCounter
	tenantRequests metric.Int64Counter
	tenant5xx      metric.Int64Counter
	tenant4xx      metric.Int64Counter
	tenantLatency  metric.Float64Histogram
}

func newHTTPServerInstruments(
	tl *tracing.Telemetry,
	enableAuthenticatedTenantMetrics bool,
) httpServerInstruments {
	// MetricsFactory presence is the canonical "metrics enabled" signal across
	// the library even though these instruments are built from MeterProvider.
	if tl == nil || tl.MeterProvider == nil || tl.MetricsFactory == nil {
		return httpServerInstruments{}
	}

	meter := tl.MeterProvider.Meter(tl.LibraryName)
	instruments := httpServerInstruments{
		duration:       newHTTPServerDurationHistogram(meter),
		activeRequests: newActiveRequestsCounter(meter),
	}

	if !enableAuthenticatedTenantMetrics {
		return instruments
	}

	instruments.tenantRequests = newAuthenticatedTenantHTTPRequestsCounter(meter)
	instruments.tenant5xx = newAuthenticatedTenantHTTPResponses5xxCounter(meter)
	instruments.tenant4xx = newAuthenticatedTenantHTTPResponses4xxCounter(meter)
	instruments.tenantLatency = newAuthenticatedTenantHTTPLatencyHistogram(meter)

	return instruments
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
// For the opt-in per-tenant variant see WithAuthenticatedTenantHTTPMetrics.
// The two are mutually exclusive - registering both double-records every metric.
func (tm *TelemetryMiddleware) WithTelemetry(tl *tracing.Telemetry, excludedRoutes ...string) fiber.Handler {
	return tm.withTelemetry(tl, false, excludedRoutes...)
}

// WithAuthenticatedTenantHTTPMetrics adds the standard HTTP telemetry and the
// separate opt-in per-tenant request, 4xx-response, and 5xx-error counters plus
// a latency histogram. The tenant metrics are recorded only when an
// authentication layer has explicitly populated the request context with
// observability.ContextWithAuthenticatedTenantID. Client-controlled headers,
// baggage, metadata, and generic span attributes are never accepted as an
// identity source.
//
// This method already includes the standard HTTP telemetry. Do NOT also register
// WithTelemetry on the same app: both would record http.server.request.duration,
// doubling every observation of the global RED metric.
func (tm *TelemetryMiddleware) WithAuthenticatedTenantHTTPMetrics(
	tl *tracing.Telemetry,
	excludedRoutes ...string,
) fiber.Handler {
	return tm.withTelemetry(tl, true, excludedRoutes...)
}

func (tm *TelemetryMiddleware) withTelemetry(
	tl *tracing.Telemetry,
	enableAuthenticatedTenantMetrics bool,
	excludedRoutes ...string,
) fiber.Handler {
	// Build the duration histogram once at handler-construction time. The
	// effective Telemetry may be supplied either via the explicit `tl` argument
	// or via the receiver's stored Telemetry, mirroring the per-request logic
	// below. If neither resolves, or any required component is nil, the
	// histogram is left nil and recording is skipped.
	bootstrapTelemetry := tl
	if bootstrapTelemetry == nil && tm != nil {
		bootstrapTelemetry = tm.Telemetry
	}

	instruments := newHTTPServerInstruments(bootstrapTelemetry, enableAuthenticatedTenantMetrics)

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
		activeDone := trackActiveRequest(c.Context(), instruments.activeRequests, method)
		defer activeDone()

		if effectiveTelemetry.TracerProvider == nil {
			returnedErr := c.Next()
			statusCode, chainErr, _ := resolveHTTPResponse(c, returnedErr)

			durationSeconds := time.Since(requestStart).Seconds()

			recordHTTPServerDuration(c, instruments.duration, method, durationSeconds, statusCode)
			recordAuthenticatedTenantHTTPMetrics(c, instruments, durationSeconds, statusCode)

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

		// Start the server span from an identity-filtered context so the
		// AttrBag span processors cannot copy tenant/customer identity onto
		// the built-in HTTP server span at start, then restore the full
		// request context (with the span attached) so downstream application
		// spans and opted-in business telemetry keep the identity attributes.
		spanStartCtx := identityFilteredSpanStartContext(traceCtx)
		_, span := tracer.Start(spanStartCtx, spanName, trace.WithSpanKind(trace.SpanKindServer))
		ctx = trace.ContextWithSpan(traceCtx, span)
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
		statusCode, chainErr, handlerErr := resolveHTTPResponse(c, returnedErr)

		applyTelemetrySpanAttributes(span, c, statusCode, telemetryRequestAttrs{
			method:         method,
			methodOriginal: methodOriginal,
			methodReplaced: methodReplaced,
			protocol:       protocol,
			hostname:       hostname,
			userAgent:      userAgent,
			handlerErr:     handlerErr,
		})

		durationSeconds := time.Since(requestStart).Seconds()

		recordHTTPServerDuration(c, instruments.duration, method, durationSeconds, statusCode)
		recordAuthenticatedTenantHTTPMetrics(c, instruments, durationSeconds, statusCode)

		return chainErr
	}
}

// httpServerSpanIdentityKeys enumerates the request-identity attribute keys
// that must never appear on the built-in HTTP server span. Infrastructure
// telemetry may only use stable transport dimensions; identity stays
// available to application spans via the unfiltered request context.
var httpServerSpanIdentityKeys = map[attribute.Key]struct{}{
	attribute.Key(constant.AttrKeyTenantID):  {},
	attribute.Key(constant.AttrKeyContextID): {},
}

// identityFilteredSpanStartContext returns a context safe to start the
// built-in HTTP server span from: request-identity attributes (tenant.id,
// context.id) are removed from the AttrBag and the tenant.id baggage member
// is dropped, so the AttrBag span processors cannot copy request identity
// onto the span at OnStart. Callers must keep using the original, unfiltered
// context for downstream work so application spans retain identity.
func identityFilteredSpanStartContext(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}

	attrs := observability.AttributesFromContext(ctx)

	filtered := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		if _, isIdentity := httpServerSpanIdentityKeys[attr.Key]; isIdentity {
			continue
		}

		filtered = append(filtered, attr)
	}

	if len(filtered) != len(attrs) {
		ctx = observability.ReplaceAttributes(ctx, filtered...)
	}

	if bag := baggage.FromContext(ctx); bag.Member(constant.AttrKeyTenantID).Key() != "" {
		ctx = baggage.ContextWithBaggage(ctx, bag.DeleteMember(constant.AttrKeyTenantID))
	}

	return ctx
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

	// The error-recording mechanism is status-driven; status itself stays
	// gated on statusCode below regardless of which branch runs, so a 4xx
	// handler error never flips the span to Error per OTel semconv:
	//   - >=500: the OTel semconv "exception" event
	//     (exception.type/exception.message), which APM backends (Tempo,
	//     Jaeger) index on - a custom-named event does not surface there.
	//     Emitted directly rather than via span.RecordError: handing
	//     req.handlerErr to RecordError is unsafe (its Error() may panic),
	//     and wrapping the sanitized message in errors.New would report
	//     exception.type "errors.errorString" for EVERY handler failure,
	//     erasing the original type an operator needs to locate the bug.
	//   - <500: the custom http.handler.error event, kept for handler errors
	//     that don't warrant "exception" status, e.g. a mapped 4xx.
	// tracing.ErrorMessage is unconditionally safe to call - it routes
	// through log.SafeErrorMessage, which never panics regardless of
	// req.handlerErr's shape (nil, typed-nil, or a valid-but-unsafe-to
	// -stringify error). errorTypeOriginal is reflect-only, so it never
	// calls Error() either.
	if req.handlerErr != nil {
		if statusCode >= 500 {
			span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
				semconv.ExceptionTypeKey.String(errorTypeOriginal(req.handlerErr)),
				semconv.ExceptionMessageKey.String(tracing.ErrorMessage(req.handlerErr)),
			))
		} else {
			tracing.HandleSpanBusinessErrorEvent(span, httpHandlerErrorEvent, req.handlerErr)
		}
	}

	if statusCode >= 500 {
		span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(statusCode))
	}
}

// httpServerDurationAttrs builds the standard HTTP server duration attribute
// set. Tenant identity is deliberately excluded: the stable semantic-convention
// metric must remain byte-for-byte compatible with its existing contract.
func httpServerDurationAttrs(c fiber.Ctx, method string, statusCode int) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 4)
	attrs = append(attrs,
		attribute.String("http.request.method", method),
		attribute.Int("http.response.status_code", statusCode),
	)

	if routePath, present := routeAttribute(c); present {
		attrs = append(attrs, attribute.String("http.route", routePath))
	}

	if errType := classifyHTTPErrorType(statusCode); errType != "" {
		attrs = append(attrs, attribute.String("error.type", errType))
	}

	return attrs
}

// recordHTTPServerDuration emits the standard http.server.request.duration
// histogram. Its name, unit, description, buckets, and attributes are unchanged.
func recordHTTPServerDuration(
	c fiber.Ctx,
	hist metric.Float64Histogram,
	method string,
	durationSeconds float64,
	statusCode int,
) {
	if hist == nil || c == nil {
		return
	}

	attrs := httpServerDurationAttrs(c, method, statusCode)
	hist.Record(c.Context(), durationSeconds, metric.WithAttributes(attrs...))
}

// recordAuthenticatedTenantHTTPMetrics emits the opt-in per-tenant metrics only
// when the application authentication layer has explicitly attested a tenant in
// the request context. Transport-controlled identity sources (headers, baggage,
// gRPC metadata, AttrBag, span attributes) are intentionally ignored.
//
// The request, 4xx-response, and 5xx-error counters carry only tenant and route.
// Separate instruments keep each at 1,500 attribute sets in the documented
// 50-tenant, 30-route scenario; adding status to one counter would create 9,000 sets and
// overflow the OTel SDK's default 2,000-set limit. The latency histogram carries
// only tenant and a bounded status class. Method, exact per-route status, and
// per-route latency remain trace-level questions.
func recordAuthenticatedTenantHTTPMetrics(
	c fiber.Ctx,
	instruments httpServerInstruments,
	durationSeconds float64,
	statusCode int,
) {
	if c == nil || (instruments.tenantRequests == nil &&
		instruments.tenant5xx == nil &&
		instruments.tenant4xx == nil &&
		instruments.tenantLatency == nil) {
		return
	}

	tenantID, ok := observability.AuthenticatedTenantIDFromContext(c.Context())
	if !ok {
		return
	}

	tenantAttr := attribute.String(constant.AttrKeyTenantID, tenantID.String())
	ctx := c.Context()
	routeAttrs := []attribute.KeyValue{
		tenantAttr,
		attribute.String("http.route", resolvedHTTPRoute(c)),
	}

	if instruments.tenantRequests != nil {
		instruments.tenantRequests.Add(ctx, 1, metric.WithAttributes(routeAttrs...))
	}

	if instruments.tenant5xx != nil && isHTTPServerError(statusCode) {
		instruments.tenant5xx.Add(ctx, 1, metric.WithAttributes(routeAttrs...))
	}

	if instruments.tenant4xx != nil && isHTTPClientError(statusCode) {
		instruments.tenant4xx.Add(ctx, 1, metric.WithAttributes(routeAttrs...))
	}

	if instruments.tenantLatency != nil {
		latencyAttrs := []attribute.KeyValue{
			tenantAttr,
			attribute.String("http.response.status_class", classifyHTTPStatusClass(statusCode)),
		}
		instruments.tenantLatency.Record(ctx, durationSeconds, metric.WithAttributes(latencyAttrs...))
	}
}

// isHTTPServerError reports whether statusCode is in the bounded HTTP 5xx class.
func isHTTPServerError(statusCode int) bool {
	return statusCode >= http.StatusInternalServerError && statusCode <= 599
}

// isHTTPClientError reports whether statusCode is in the bounded HTTP 4xx class.
func isHTTPClientError(statusCode int) bool {
	return statusCode >= http.StatusBadRequest && statusCode <= 499
}

// classifyHTTPStatusClass bounds arbitrary Fiber status integers to six stable
// values. Exact status codes remain available on the standard metric and traces.
func classifyHTTPStatusClass(statusCode int) string {
	if statusCode < 100 || statusCode > 599 {
		return "other"
	}

	return strconv.Itoa(statusCode/100) + "xx"
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
