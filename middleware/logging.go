package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	observability "github.com/LerianStudio/lib-observability"
	constant "github.com/LerianStudio/lib-observability/constants"
	obslog "github.com/LerianStudio/lib-observability/log"
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
)

const (
	headerReferer     = "Referer"
	headerContentType = "Content-Type"

	maxObfuscationDepth = 32
)

var logObfuscationDisabled = os.Getenv("LOG_OBFUSCATION_DISABLED") == "true"

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
	Body          string
}

// ResponseMetricsWrapper stores response metadata used to finish RequestInfo.
type ResponseMetricsWrapper struct {
	Context    *fiber.Ctx
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
	Logger              obslog.Logger
	ObfuscationDisabled bool
	ExcludedRoutes      []string
}

// LogMiddlewareOption configures HTTP and gRPC logging middleware.
type LogMiddlewareOption func(l *logMiddleware)

// WithCustomLogger configures a custom logger for access logging.
func WithCustomLogger(logger obslog.Logger) LogMiddlewareOption {
	return func(l *logMiddleware) {
		if !isNilLogger(logger) {
			l.Logger = logger
		}
	}
}

// WithObfuscationDisabled disables request body obfuscation.
func WithObfuscationDisabled(disabled bool) LogMiddlewareOption {
	return func(l *logMiddleware) {
		l.ObfuscationDisabled = disabled
	}
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
		Logger:              &obslog.GoLogger{},
		ObfuscationDisabled: logObfuscationDisabled,
		ExcludedRoutes:      append([]string(nil), defaultLogExcludedRoutes...),
	}

	for _, opt := range opts {
		opt(mid)
	}

	return mid
}

func isNilLogger(logger obslog.Logger) bool {
	if logger == nil {
		return true
	}

	value := reflect.ValueOf(logger)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// NewRequestInfo creates RequestInfo from a Fiber context.
func NewRequestInfo(c *fiber.Ctx, obfuscationDisabled bool) *RequestInfo {
	if c == nil {
		return &RequestInfo{Date: time.Now().UTC()}
	}

	username, referer := "-", "-"
	rawURL := string(c.Request().URI().FullURI())

	parsedURL, err := url.Parse(rawURL)
	if err == nil && parsedURL.User != nil {
		if name := parsedURL.User.Username(); name != "" {
			username = name
		}
	}

	if c.Get(headerReferer) != "" {
		referer = sanitizeReferer(c.Get(headerReferer))
	}

	body := ""
	bodyBytes := c.Body()

	if len(bodyBytes) > 0 {
		if !obfuscationDisabled {
			body = getBodyObfuscatedString(c, bodyBytes)
		} else {
			body = string(bodyBytes)
		}
	}

	return &RequestInfo{
		TraceID:       c.Get(headerID),
		Method:        c.Method(),
		URI:           sanitizeURL(c.OriginalURL()),
		Username:      username,
		Referer:       referer,
		UserAgent:     sanitizeLogValue(c.Get(headerUserAgent)),
		RemoteAddress: c.IP(),
		Protocol:      c.Protocol(),
		Date:          time.Now().UTC(),
		Body:          body,
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
		strconv.Itoa(r.Size),
		sanitizeLogValue(r.Referer),
		sanitizeLogValue(r.UserAgent),
	}, " ")
}

func (r *RequestInfo) String() string {
	return r.CLFString()
}

// FinishRequestInfo fills response status, body size, and duration.
func (r *RequestInfo) FinishRequestInfo(rw *ResponseMetricsWrapper) {
	if rw == nil {
		return
	}

	r.Duration = time.Since(r.Date)
	r.Status = rw.StatusCode
	r.Size = rw.Size
}

// WithHTTPLogging logs Fiber HTTP access requests.
//
// By default the probe paths /health and /readyz, the Prometheus scrape
// path /metrics, and Swagger asset routes are skipped. Use
// WithExcludedRoutes to suppress additional paths without losing the
// defaults.
func WithHTTPLogging(opts ...LogMiddlewareOption) fiber.Handler {
	return func(c *fiber.Ctx) error {
		mid := buildOpts(opts...)

		if isRouteExcludedFromList(c, mid.ExcludedRoutes) {
			return c.Next()
		}

		if strings.Contains(c.Path(), "swagger") && c.Path() != "/swagger/index.html" {
			return c.Next()
		}

		setRequestHeaderID(c)

		info := NewRequestInfo(c, mid.ObfuscationDisabled)

		requestID := c.Get(headerID)
		ctx := c.UserContext()

		if tenantID := ResolveTenantIDFromHTTP(c); tenantID != "" {
			ctx = observability.ContextWithSpanAttributes(ctx, attribute.String(constant.AttrKeyTenantID, tenantID))
		}

		logger := mid.Logger.
			With(obslog.String(headerID, info.TraceID)).
			With(obslog.String("message_prefix", requestID+constant.LoggerDefaultSeparator))

		if tenantID := resolveTenantIDForTelemetry(ctx); tenantID != "" {
			logger = logger.With(obslog.String(constant.AttrKeyTenantID, tenantID))
		}

		ctx = observability.ContextWithLogger(ctx, logger)
		c.SetUserContext(ctx)

		err := c.Next()
		statusCode := httpStatusCode(c, err)

		rw := ResponseMetricsWrapper{
			Context:    c,
			StatusCode: statusCode,
			Size:       len(c.Response().Body()),
		}

		info.FinishRequestInfo(&rw)

		accessLogger := logger.With(obslog.String("http_client_ip", info.RemoteAddress))
		if body := errorBodyForLog(c, statusCode, mid.ObfuscationDisabled); body != "" {
			accessLogger = accessLogger.With(obslog.String("http_error", body))
		}

		accessLogger.Log(c.UserContext(), obslog.LevelInfo, info.CLFString())

		return err
	}
}

// maxErrorBodyLogLen caps the http_error field so a large error response body
// cannot bloat the access log entry. 2 KiB comfortably covers structured API
// error payloads while bounding log volume.
const maxErrorBodyLogLen = 2048

// errorBodyForLog returns a sanitized, length-capped copy of the response body
// suitable for the http_error log field, or "" when it should not be logged.
//
// Bodies are only logged for error responses (status >= 400). The body is
// obfuscated with the same content-type-aware redaction pipeline used for
// request bodies (unless obfuscation is disabled), so sensitive fields are not
// leaked, and is then truncated to maxErrorBodyLogLen bytes on a UTF-8 rune
// boundary.
func errorBodyForLog(c *fiber.Ctx, statusCode int, obfuscationDisabled bool) string {
	if c == nil || statusCode < fiber.StatusBadRequest {
		return ""
	}

	bodyBytes := c.Response().Body()
	if len(bodyBytes) == 0 {
		return ""
	}

	body := string(bodyBytes)
	if !obfuscationDisabled {
		body = getResponseBodyObfuscatedString(c, bodyBytes)
	}

	return truncateLogBody(body)
}

// truncateLogBody caps body at maxErrorBodyLogLen bytes, cutting on a rune
// boundary so the stored value is always valid UTF-8.
func truncateLogBody(body string) string {
	if len(body) <= maxErrorBodyLogLen {
		return body
	}

	lastFit := 0

	for i := range body {
		if i > maxErrorBodyLogLen {
			return body[:lastFit]
		}

		lastFit = i
	}

	return body[:lastFit]
}

func httpStatusCode(c *fiber.Ctx, err error) int {
	statusCode := c.Response().StatusCode()
	if err == nil {
		return statusCode
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return fiberErr.Code
	}

	if statusCode < fiber.StatusBadRequest {
		return fiber.StatusInternalServerError
	}

	return statusCode
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

func handleJSONBody(bodyBytes []byte) string {
	var bodyData any
	if err := json.Unmarshal(bodyBytes, &bodyData); err != nil {
		return redactedBody
	}

	switch v := bodyData.(type) {
	case map[string]any:
		obfuscateMapRecursively(v, 0)
	case []any:
		obfuscateSliceRecursively(v, 0)
	default:
		return redactedBody
	}

	updatedBody, err := json.Marshal(bodyData)
	if err != nil {
		return redactedBody
	}

	return string(updatedBody)
}
