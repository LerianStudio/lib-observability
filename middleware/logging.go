package middleware

import (
	"context"
	"strconv"
	"strings"
	"time"

	observability "github.com/LerianStudio/lib-observability/v3"
	constant "github.com/LerianStudio/lib-observability/v3/constants"
	"github.com/LerianStudio/lib-observability/v3/grpcmiddleware"
	obslog "github.com/LerianStudio/lib-observability/v3/log"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
)

const (
	headerReferer           = "Referer"
	unknownResponseBodySize = -1
)

// RequestInfo stores HTTP access log data.
type RequestInfo struct {
	Method        string
	Username      string
	URI           string
	Referer       string
	RemoteAddress string
	Status        int
	Date          time.Time
	Duration      time.Duration
	UserAgent     string
	TraceID       string
	Protocol      string
	Size          int
	// Deprecated: HTTP request bodies are never captured by access logging.
	Body  string
	start time.Time
}

// ResponseMetricsWrapper stores response metadata used to finish RequestInfo.
type ResponseMetricsWrapper struct {
	Context    fiber.Ctx
	StatusCode int
	Size       int
}

// defaultLogExcludedRoutes is the canonical set of probe and scrape paths
// that are suppressed from access logging by default. Readiness probes and
// Prometheus scrapes fire every few seconds per pod and would otherwise
// dominate the access log. Failures still surface through the per-route
// observability emitted by the handler itself (e.g. the "readyz_unhealthy"
// Warn entry on 503).
var defaultLogExcludedRoutes = []string{"/health", "/readyz", "/metrics"}

type logMiddleware struct {
	Logger         obslog.Logger
	ExcludedRoutes []string
}

// LogMiddlewareOption configures HTTP and gRPC logging middleware.
type LogMiddlewareOption func(l *logMiddleware)

// WithCustomLogger configures a custom logger for access logging.
func WithCustomLogger(logger obslog.Logger) LogMiddlewareOption {
	return func(l *logMiddleware) {
		if !obslog.IsNil(logger) {
			l.Logger = logger
		}
	}
}

// WithObfuscationDisabled is retained for source compatibility.
//
// Deprecated: HTTP request bodies are never captured by access logging.
func WithObfuscationDisabled(_ bool) LogMiddlewareOption {
	return func(*logMiddleware) {}
}

// WithExcludedRoutes suppresses access logs for any request whose path is
// prefixed by one of the supplied routes. Matches the prefix semantics used
// by TelemetryMiddleware.WithTelemetry so a single env-driven list can be
// threaded through both middlewares. Repeated calls append.
func WithExcludedRoutes(routes ...string) LogMiddlewareOption {
	return func(l *logMiddleware) {
		for _, r := range routes {
			if r == "" {
				continue
			}

			l.ExcludedRoutes = append(l.ExcludedRoutes, r)
		}
	}
}

func buildOpts(opts ...LogMiddlewareOption) *logMiddleware {
	mid := &logMiddleware{
		// LevelInfo, not the zero value: an uninitialized GoLogger{} defaults
		// to LevelError, so the access log would emit only 5xx responses -
		// silently dropping every 2xx/3xx/4xx line - for any caller that
		// doesn't supply WithCustomLogger.
		Logger:         &obslog.GoLogger{Level: obslog.LevelInfo},
		ExcludedRoutes: append([]string(nil), defaultLogExcludedRoutes...),
	}

	for _, opt := range opts {
		opt(mid)
	}

	return mid
}

// NewRequestInfo creates RequestInfo from a Fiber context.
//
// The URI field is resolved from Fiber's route match state. Callers that
// construct it before c.Next() must invoke FinishRequestInfo after the
// downstream handler completes to finalize the route template.
// The second argument is retained for source compatibility and has no effect.
func NewRequestInfo(c fiber.Ctx, _ bool) *RequestInfo {
	now := time.Now()
	if c == nil {
		return &RequestInfo{Date: now.UTC(), start: now}
	}

	return &RequestInfo{
		TraceID:       c.Get(headerID),
		Method:        c.Method(),
		URI:           resolvedHTTPRoute(c),
		Username:      "-",
		Referer:       "-",
		UserAgent:     sanitizeLogValue(c.Get(headerUserAgent)),
		RemoteAddress: c.IP(),
		Protocol:      c.Protocol(),
		Date:          now.UTC(),
		start:         now,
	}
}

// CLFString returns a Common Log Format style access log entry.
func (r *RequestInfo) CLFString() string {
	return strings.Join([]string{
		sanitizeLogValue(r.RemoteAddress),
		"-",
		sanitizeLogValue(r.Username),
		sanitizeLogValue(r.Protocol),
		r.Date.Format("[02/Jan/2006:15:04:05 -0700]"),
		`"` + sanitizeLogValue(r.Method) + " " + sanitizeLogValue(r.URI) + `"`,
		strconv.Itoa(r.Status),
		clfBodySize(r.Size),
		sanitizeLogValue(r.Referer),
		sanitizeLogValue(r.UserAgent),
	}, " ")
}

// clfBodySize renders the response body size field per CLF convention: "-"
// for an unknown size (unknownResponseBodySize, e.g. a streamed response with
// no declared Content-Length), the byte count otherwise. A literal "-1" is
// not a CLF convention and reads as a negative byte count to any log parser.
func clfBodySize(size int) string {
	if size == unknownResponseBodySize {
		return "-"
	}

	return strconv.Itoa(size)
}

func (r *RequestInfo) String() string {
	return r.CLFString()
}

// FinishRequestInfo fills response status, body size, and duration.
func (r *RequestInfo) FinishRequestInfo(rw *ResponseMetricsWrapper) {
	if rw == nil {
		return
	}

	if !r.start.IsZero() {
		r.Duration = time.Since(r.start)
	} else if !r.Date.IsZero() {
		r.Duration = time.Since(r.Date)
	}

	r.Status = rw.StatusCode
	r.Size = rw.Size
	r.URI = resolvedHTTPRoute(rw.Context)
}

// WithHTTPLogging logs Fiber HTTP access requests.
//
// By default the probe paths /health and /readyz, the Prometheus scrape
// path /metrics, and Swagger asset routes are skipped. Use
// WithExcludedRoutes to suppress additional paths without losing the
// defaults.
func WithHTTPLogging(opts ...LogMiddlewareOption) fiber.Handler {
	return func(c fiber.Ctx) error {
		mid := buildOpts(opts...)

		if isRouteExcludedFromList(c, mid.ExcludedRoutes) {
			return nextWithNormalizedHTTPError(c)
		}

		if strings.Contains(c.Path(), "swagger") && c.Path() != "/swagger/index.html" {
			return nextWithNormalizedHTTPError(c)
		}

		setRequestHeaderID(c)

		info := NewRequestInfo(c, false)

		requestID := c.Get(headerID)
		ctx := c.Context()

		logger := mid.Logger.
			With(obslog.String(headerID, info.TraceID)).
			With(obslog.String("message_prefix", requestID+constant.LoggerDefaultSeparator))

		ctx = observability.ContextWithLogger(ctx, logger)
		c.SetContext(ctx)

		returnedErr := c.Next()
		statusCode, chainErr, handlerErr := resolveHTTPResponse(c, returnedErr)

		rw := ResponseMetricsWrapper{
			Context:    c,
			StatusCode: statusCode,
			Size:       responseBodySize(c),
		}

		info.FinishRequestInfo(&rw)

		fields := []obslog.Field{
			obslog.Int("http_status_code", info.Status),
			obslog.String("http_method", info.Method),
			obslog.String("http_path", info.URI),
			obslog.Int("http_latency_ms", int(info.Duration.Milliseconds())),
		}
		if handlerErr != nil {
			// tracing.ErrorMessage, not obslog.Err(handlerErr) storing the raw
			// error: the access log's error text must match what the span
			// records - same Bearer/Basic redaction, same length cap - and
			// must never hand a raw error straight to a log sink that a
			// different backend might stringify unguarded.
			fields = append(fields, obslog.String("error", tracing.ErrorMessage(handlerErr)))
		}

		logger.With(fields...).Log(c.Context(), httpAccessLogLevel(info.Status), info.CLFString())

		return chainErr
	}
}

func responseBodySize(c fiber.Ctx) int {
	response := c.Response()
	if !response.IsBodyStream() {
		return len(response.Body())
	}

	if size := response.Header.ContentLength(); size >= 0 {
		return size
	}

	return unknownResponseBodySize
}

func nextWithNormalizedHTTPError(c fiber.Ctx) error {
	return normalizeHTTPHandlerError(c.Next())
}

func httpAccessLogLevel(statusCode int) obslog.Level {
	switch {
	case statusCode >= fiber.StatusInternalServerError:
		return obslog.LevelError
	case statusCode >= fiber.StatusBadRequest:
		return obslog.LevelWarn
	default:
		return obslog.LevelInfo
	}
}

func httpStatusCode(c fiber.Ctx, err error) int {
	statusCode := c.Response().StatusCode()

	if err == nil {
		return statusCode
	}

	// asFiberError rather than errors.As: a typed-nil *fiber.Error earlier in
	// a joined chain must not shadow a valid one sitting later in it.
	if fiberErr := asFiberError(err); fiberErr != nil {
		return fiberErr.Code
	}

	// A non-fiber error on this no-finalizer path reaches Fiber's default
	// ErrorHandler, which renders 500 to the client regardless of any status
	// the handler already wrote - the log must report what the client saw.
	return fiber.StatusInternalServerError
}

// normalizeHTTPHandlerError checks ONLY the top-level error value, mirroring
// Fiber v3's own internal/nilerror check on the value a handler returns.
//
// It deliberately does NOT walk Unwrap()/Unwrap() []error: a valid, non-nil
// wrapper whose Unwrap() chain happens to bottom out on a typed-nil (e.g. a
// domain error struct with a nil Cause, or errors.Join pairing a real error
// with an unrelated nil one) is not itself unsafe to hand to the rest of the
// pipeline - the wrapper's own Error() is what gets called, and a well-formed
// wrapper does not blindly delegate to a nil child. Recursing into the chain
// here previously meant one nil-chained error anywhere below a perfectly good
// top-level error (e.g. errors.Join(fiber.NewError(400, ...), typedNil))
// collapsed the whole response to a 500, discarding a correctly mapped 4xx.
func normalizeHTTPHandlerError(err error) error {
	if err == nil {
		return nil
	}

	if obslog.IsNil(err) {
		return fiber.ErrInternalServerError
	}

	return err
}

// WithGrpcLogging logs gRPC unary requests and attaches a request-scoped logger to context.
func WithGrpcLogging(opts ...LogMiddlewareOption) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		ctx = normalizeGRPCContext(ctx)
		requestID := resolveGRPCRequestID(ctx, req)

		if rid, ok := getValidBodyRequestID(req); ok {
			if prev := getMetadataID(ctx); prev != "" && prev != rid {
				mid := buildOpts(opts...)
				mid.Logger.Log(ctx, obslog.LevelDebug, "Overriding correlation id from metadata with body request_id",
					obslog.String("metadata_id", prev),
					obslog.String("body_request_id", rid),
				)
			}
		}

		ctx = observability.ContextWithHeaderID(ctx, requestID)
		ctx = observability.ContextWithSpanAttributes(ctx, attribute.String("app.request.request_id", requestID))

		if tenantID := ResolveTenantIDFromGRPC(ctx); tenantID != "" {
			ctx = observability.ContextWithSpanAttributes(ctx, attribute.String(constant.AttrKeyTenantID, tenantID))
		}

		_, _, reqID, _ := observability.NewTrackingFromContext(ctx)

		mid := buildOpts(opts...)
		logger := mid.Logger.
			With(obslog.String(headerID, reqID)).
			With(obslog.String("message_prefix", reqID+constant.LoggerDefaultSeparator))

		if tenantID := resolveTenantIDForTelemetry(ctx); tenantID != "" {
			logger = logger.With(obslog.String(constant.AttrKeyTenantID, tenantID))
		}

		ctx = observability.ContextWithLogger(ctx, logger)

		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start)
		// Same death path as grpcmiddleware.NormalizeGRPCError guards
		// against: whatever this interceptor returns goes straight to
		// grpc-go's own dispatch, which calls status.FromError - and, on its
		// fallback path, err.Error() unconditionally. Proves stringifiability
		// rather than inspecting shape, so it also catches a valid, non-nil
		// error whose Unwrap chain hits an unguarded typed-nil (errors.Join,
		// a delegating wrapper), not just a bare top-level one. Shared with
		// the grpcmiddleware package's own server/client interceptors rather
		// than duplicated - unlike the small per-request helpers below
		// (resolveGRPCRequestID, getMetadataID), which stay duplicated to
		// keep this package's only gRPC coupling at the
		// grpc.UnaryServerInterceptor type.
		err = grpcmiddleware.NormalizeGRPCError(err)

		methodName := "unknown"
		if info != nil {
			methodName = info.FullMethod
		}

		fields := []obslog.Field{
			obslog.String("method", methodName),
			obslog.String("duration", duration.String()),
		}
		if err != nil {
			fields = append(fields, obslog.Err(err))
		}

		logger.Log(ctx, obslog.LevelInfo, "gRPC request finished", fields...)

		return resp, err
	}
}
