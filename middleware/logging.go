package middleware

import (
	"context"
	"encoding/json"
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

type logMiddleware struct {
	Logger              obslog.Logger
	ObfuscationDisabled bool
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

func buildOpts(opts ...LogMiddlewareOption) *logMiddleware {
	mid := &logMiddleware{
		Logger:              &obslog.GoLogger{},
		ObfuscationDisabled: logObfuscationDisabled,
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

	if c.Request().Header.ContentLength() > 0 {
		bodyBytes := c.Body()
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
func WithHTTPLogging(opts ...LogMiddlewareOption) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if c.Path() == "/health" {
			return c.Next()
		}

		if strings.Contains(c.Path(), "swagger") && c.Path() != "/swagger/index.html" {
			return c.Next()
		}

		setRequestHeaderID(c)

		mid := buildOpts(opts...)
		info := NewRequestInfo(c, mid.ObfuscationDisabled)

		requestID := c.Get(headerID)
		logger := mid.Logger.
			With(obslog.String(headerID, info.TraceID)).
			With(obslog.String("message_prefix", requestID+constant.LoggerDefaultSeparator))

		ctx := observability.ContextWithLogger(c.UserContext(), logger)
		c.SetUserContext(ctx)

		err := c.Next()

		rw := ResponseMetricsWrapper{
			Context:    c,
			StatusCode: c.Response().StatusCode(),
			Size:       len(c.Response().Body()),
		}

		info.FinishRequestInfo(&rw)
		logger.Log(c.UserContext(), obslog.LevelInfo, info.CLFString())

		return err
	}
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

		_, _, reqID, _ := observability.NewTrackingFromContext(ctx)

		mid := buildOpts(opts...)
		logger := mid.Logger.
			With(obslog.String(headerID, reqID)).
			With(obslog.String("message_prefix", reqID+constant.LoggerDefaultSeparator))

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
		return string(bodyBytes)
	}

	switch v := bodyData.(type) {
	case map[string]any:
		obfuscateMapRecursively(v, 0)
	case []any:
		obfuscateSliceRecursively(v, 0)
	default:
		return string(bodyBytes)
	}

	updatedBody, err := json.Marshal(bodyData)
	if err != nil {
		return string(bodyBytes)
	}

	return string(updatedBody)
}
