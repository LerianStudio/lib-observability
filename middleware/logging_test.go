//go:build unit

package middleware

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	observability "github.com/LerianStudio/lib-observability/v3"
	obslog "github.com/LerianStudio/lib-observability/v3/log"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type captureLogger struct {
	mu       sync.Mutex
	messages []string
	fields   []obslog.Field
	levels   []int
}

func (l *captureLogger) Log(_ context.Context, level int, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.messages = append(l.messages, msg)
	l.fields = append(l.fields, obslog.Fields(fields...)...)
	l.levels = append(l.levels, level)
}

func (l *captureLogger) With(fields ...any) obslog.Logger {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.fields = append(l.fields, obslog.Fields(fields...)...)

	return l
}

func (l *captureLogger) WithGroup(_ string) obslog.Logger { return l }

func (l *captureLogger) Enabled(_ int) bool { return true }

func (l *captureLogger) Sync(_ context.Context) error { return nil }

func (l *captureLogger) snapshot() ([]string, []obslog.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()

	messages := append([]string(nil), l.messages...)
	fields := append([]obslog.Field(nil), l.fields...)

	return messages, fields
}

func (l *captureLogger) levelSnapshot() []int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]int(nil), l.levels...)
}

type grpcRequestWithID struct {
	RequestId string
}

type typedNilCustomError struct {
	message string
}

func (e *typedNilCustomError) Error() string {
	return e.message
}

type typedNilMapError map[string]string

func (e typedNilMapError) Error() string {
	return e["message"]
}

type typedNilSliceError []string

func (e typedNilSliceError) Error() string {
	if len(e) == 0 {
		return ""
	}

	return e[0]
}

type typedNilFuncError func() string

func (e typedNilFuncError) Error() string {
	if e == nil {
		return ""
	}

	return e()
}

type typedNilChannelError chan string

func (typedNilChannelError) Error() string {
	return "channel error"
}

type staticWrappedError struct {
	err error
}

type trackedResponseStream struct {
	reader                    *strings.Reader
	loggingReturned           *atomic.Bool
	readBeforeLoggingReturned atomic.Bool
	bytesRead                 atomic.Int64
}

func (s *trackedResponseStream) Read(buffer []byte) (int, error) {
	if !s.loggingReturned.Load() {
		s.readBeforeLoggingReturned.Store(true)
	}

	read, err := s.reader.Read(buffer)
	s.bytesRead.Add(int64(read))

	return read, err
}

func (e staticWrappedError) Error() string {
	return "wrapped error"
}

func (e staticWrappedError) Unwrap() error {
	return e.err
}

func (r grpcRequestWithID) GetRequestId() string {
	return r.RequestId
}

// TestCLFBodySize_UsesDashForUnknownSize verifies the CLF body-size field
// prints "-" (the Common Log Format convention for an unknown value) rather
// than a literal "-1", which reads as a negative byte count to any CLF
// parser.
func TestCLFBodySize_UsesDashForUnknownSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		size int
		want string
	}{
		{name: "unknown size", size: unknownResponseBodySize, want: "-"},
		{name: "zero bytes", size: 0, want: "0"},
		{name: "known size", size: 42, want: "42"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, clfBodySize(tt.size))
		})
	}
}

func TestNewRequestInfoUsesRouteTemplateAndDoesNotCaptureRequestBody(t *testing.T) {
	app := fiber.New()
	app.Post("/charge", func(c fiber.Ctx) error {
		info := NewRequestInfo(c, false)

		assert.Equal(t, "POST", info.Method)
		assert.Equal(t, "/charge", info.URI)
		assert.Equal(t, "-", info.Referer)
		assert.Equal(t, "agent", info.UserAgent)
		assert.Empty(t, info.Body)
		assert.JSONEq(t, "{\"nested\":{\"secret\":\"value\"},\"password\":\"secret\"}", string(c.Body()))

		return c.SendStatus(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/charge?name=alice&password=secret", strings.NewReader("{\"password\":\"secret\",\"nested\":{\"secret\":\"value\"}}"))
	req.Header.Set(fiber.HeaderContentType, "Application/JSON")
	req.Header.Set(headerReferer, "https://user:pass@example.com/path?token=secret#fragment")
	req.Header.Set(headerUserAgent, "agent")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestWithHTTPLoggingPreservesStreamedRequestBody(t *testing.T) {
	t.Parallel()

	const payload = `{"transaction":"streamed"}`

	logger := &captureLogger{}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Request().SetBodyStream(strings.NewReader(payload), len(payload))

		return c.Next()
	})
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Post("/stream", func(c fiber.Ctx) error {
		assert.True(t, c.Request().IsBodyStream(), "logging middleware must not materialize the request stream")

		body, err := io.ReadAll(c.Request().BodyStream())
		require.NoError(t, err)
		assert.Equal(t, payload, string(body))

		return c.SendStatus(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/stream", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestWithHTTPLoggingPreservesStreamingResponses(t *testing.T) {
	t.Parallel()

	const payload = "first chunk\nsecond chunk\n"

	tests := []struct {
		name           string
		declaredSize   int
		wantLoggedSize int
	}{
		{name: "known content length", declaredSize: len(payload), wantLoggedSize: len(payload)},
		{name: "unknown content length", declaredSize: -1, wantLoggedSize: -1},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			var loggingReturned atomic.Bool
			stream := &trackedResponseStream{
				reader:          strings.NewReader(payload),
				loggingReturned: &loggingReturned,
			}

			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				err := c.Next()
				loggingReturned.Store(true)

				return err
			})
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Get("/stream", func(c fiber.Ctx) error {
				if tt.declaredSize >= 0 {
					return c.SendStream(stream, tt.declaredSize)
				}

				return c.SendStream(stream)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/stream", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			assert.Equal(t, payload, string(body))
			assert.Equal(t, int64(len(payload)), stream.bytesRead.Load(), "the stream must be consumed exactly once")
			assert.False(t, stream.readBeforeLoggingReturned.Load(), "access logging must not consume the response stream")

			messages, _ := logger.snapshot()
			require.Len(t, messages, 1)
			assert.Contains(t, messages[0], `"GET /stream" 200 `+clfBodySize(tt.wantLoggedSize)+" ")
		})
	}
}

func TestWithHTTPLoggingAttachesLoggerAndLogsAccessEntry(t *testing.T) {
	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/ok", func(c fiber.Ctx) error {
		_, _, requestID, _ := observability.NewTrackingFromContext(c.Context())
		assert.NotEmpty(t, requestID)
		assert.Same(t, logger, observability.NewLoggerFromContext(c.Context()))

		return c.SendString("ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(body))
	assert.NotEmpty(t, resp.Header.Get(headerID))

	messages, fields := logger.snapshot()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "GET /ok")
	assert.Contains(t, messages[0], `"GET /ok" 200 2 `)
	assert.Contains(t, fields, obslog.String(headerID, resp.Header.Get(headerID)))

	var latencyFound bool

	for _, f := range fields {
		if f.Key != "http_latency_ms" {
			continue
		}

		latencyFound = true

		ms, ok := f.Value.(int)
		require.True(t, ok, "http_latency_ms should be an int")
		assert.GreaterOrEqual(t, ms, 0)
	}

	assert.True(t, latencyFound, "expected http_latency_ms field on access log entry")
}

// TestWithHTTPLoggingDefaultLoggerEmitsOn2xx verifies the DEFAULT access
// logger (no WithCustomLogger supplied - buildOpts's zero-configuration
// path) actually emits a line for a successful request. Before this fix,
// buildOpts defaulted to a bare GoLogger{} (Level: its Go zero value,
// LevelError), so Enabled(LevelInfo) was always false and every
// 2xx/3xx/4xx request produced no access log line at all - only 5xx did.
func TestWithHTTPLoggingDefaultLoggerEmitsOn2xx(t *testing.T) {
	// Not parallel: mutates the process-global stdlib log output (GoLogger
	// writes through log.Print).
	var buf bytes.Buffer

	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	app := fiber.New()
	app.Use(WithHTTPLogging())
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ok", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Contains(t, buf.String(), "GET /ok",
		"the default access logger must emit a line for a successful (2xx) request")
}

func TestHTTPRequestIDIsCanonicalAndStableAcrossTelemetrySurfaces(t *testing.T) {
	t.Parallel()
	const maxTestRequestIDLength = 128

	tests := []struct {
		name       string
		rawID      string
		wantLength int
		wantUUID   bool
		wantExact  string
	}{
		{
			name:       "overlong identifier is bounded",
			rawID:      strings.Repeat("a", 200),
			wantLength: maxTestRequestIDLength,
		},
		{
			// fasthttp's own header layer replaces embedded CR/LF in a header
			// value with a space before this middleware ever sees it (that
			// substitution is exercised directly, bytes intact, in
			// TestNormalizeRequestID_PunctuationPassesThroughIntact). What this
			// round trip verifies is the other half of the contract: every
			// printable ASCII character - including the punctuation a caller's
			// own ID format may use - passes through intact rather than being
			// rewritten, so cross-service log correlation joins survive.
			name:      "punctuation passes through intact",
			rawID:     "client id,with/unsafe;separators:and+base64=values",
			wantExact: "client id,with/unsafe;separators:and+base64=values",
		},
		{
			name:     "identifier with only control bytes is replaced with UUID",
			rawID:    "\r\n\x00\x01\x02",
			wantUUID: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tel, _, spanExp := newTelemetryHarness(t)
			logger := &captureLogger{}
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				c.Request().Header.Set(headerID, tt.rawID)

				return c.Next()
			})
			app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))

			var (
				requestHeaderID string
				contextID       string
				contextAttrID   string
			)
			app.Get("/ids", func(c fiber.Ctx) error {
				requestHeaderID = c.Get(headerID)
				_, _, contextID, _ = observability.NewTrackingFromContext(c.Context())

				for _, attr := range observability.AttributesFromContext(c.Context()) {
					if string(attr.Key) == "app.request.request_id" {
						contextAttrID = attr.Value.AsString()
						break
					}
				}

				return c.SendStatus(http.StatusNoContent)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ids", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			effectiveID := resp.Header.Get(headerID)
			require.NotEmpty(t, effectiveID)
			assert.LessOrEqual(t, len(effectiveID), maxTestRequestIDLength)
			assert.LessOrEqual(t, utf8.RuneCountInString(effectiveID), maxTestRequestIDLength)
			assert.NotContains(t, effectiveID, "\r")
			assert.NotContains(t, effectiveID, "\n")
			assert.NotContains(t, effectiveID, "\x00")
			if tt.wantLength > 0 {
				assert.Len(t, effectiveID, tt.wantLength)
			}
			if tt.wantExact != "" {
				assert.Equal(t, tt.wantExact, effectiveID)
			}
			if tt.wantUUID {
				_, err = uuid.Parse(effectiveID)
				require.NoError(t, err)
			}

			assert.Equal(t, effectiveID, requestHeaderID)
			assert.Equal(t, effectiveID, contextID)
			assert.Equal(t, effectiveID, contextAttrID)

			spans := spanExp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, effectiveID, getSpanAttr(spans[0], "app.request.request_id"))

			_, fields := logger.snapshot()
			assert.Contains(t, fields, obslog.String(headerID, effectiveID))
			assert.Contains(t, fields, obslog.String("message_prefix", effectiveID+" | "))
		})
	}
}

func TestWithHTTPLoggingIgnoresTypedNilLogger(t *testing.T) {
	var logger *captureLogger
	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/ok", func(c fiber.Ctx) error {
		assert.NotNil(t, observability.NewLoggerFromContext(c.Context()))
		return c.SendStatus(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestWithHTTPLoggingSkipsDefaultProbePaths(t *testing.T) {
	probePaths := []string{"/health", "/readyz", "/metrics"}

	for _, path := range probePaths {
		t.Run(path, func(t *testing.T) {
			logger := &captureLogger{}
			app := fiber.New()
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Get(path, func(c fiber.Ctx) error {
				return c.SendStatus(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, path, nil)
			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			messages, _ := logger.snapshot()
			assert.Empty(t, messages, "no access log expected for default probe path %s", path)
		})
	}
}

func TestWithHTTPLoggingExcludedRoutesOptionSuppressesLogs(t *testing.T) {
	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(
		WithCustomLogger(logger),
		WithExcludedRoutes("/internal"),
	))
	app.Get("/internal/diag", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/internal/diag", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	messages, _ := logger.snapshot()
	assert.Empty(t, messages, "prefix-excluded path should not produce an access log")
}

func TestWithHTTPLoggingExcludedRoutesOptionPreservesDefaults(t *testing.T) {
	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(
		WithCustomLogger(logger),
		WithExcludedRoutes("/metrics"),
	))
	app.Get("/readyz", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	messages, _ := logger.snapshot()
	assert.Empty(t, messages, "supplying custom excluded routes must not remove the default probe skip set")
}

func TestWithHTTPLoggingExcludedRoutesIgnoresEmptyStrings(t *testing.T) {
	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(
		WithCustomLogger(logger),
		WithExcludedRoutes("", "/skip"),
	))
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	messages, _ := logger.snapshot()
	require.Len(t, messages, 1, "empty exclusion entries must not swallow every request")
	assert.Contains(t, messages[0], "GET /ok")
}

func TestWithHTTPLoggingLogsErrorStatus(t *testing.T) {
	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/fail", func(fiber.Ctx) error {
		return errors.New("handler failed")
	})

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	messages, fields := logger.snapshot()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "GET /fail")
	assert.Contains(t, messages[0], " 500 ")

	var errField *obslog.Field

	for i := range fields {
		if fields[i].Key == "error" {
			errField = &fields[i]
			break
		}
	}

	require.NotNil(t, errField, "access log must carry a structured error field when the handler failed")
	gotMsg, ok := errField.Value.(string)
	require.True(t, ok, "the error field must carry tracing.ErrorMessage's sanitized string, matching what the span records")
	assert.Equal(t, "handler failed", gotMsg)
}

// TestWithHTTPLoggingOmitsErrorFieldOnSuccess guards against the structured
// error field appearing on a successful request, which would misrepresent a
// clean response as having failed.
func TestWithHTTPLoggingOmitsErrorFieldOnSuccess(t *testing.T) {
	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	_, fields := logger.snapshot()
	for _, field := range fields {
		assert.NotEqual(t, "error", field.Key, "no error field expected on a successful request")
	}
}

func TestWithHTTPLoggingUsesStatusAwareAccessLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		wantLevel int
	}{
		{name: "2xx is info", status: http.StatusNoContent, wantLevel: obslog.LevelInfo},
		{name: "3xx is info", status: http.StatusTemporaryRedirect, wantLevel: obslog.LevelInfo},
		{name: "4xx is warn", status: http.StatusBadRequest, wantLevel: obslog.LevelWarn},
		{name: "5xx is error", status: http.StatusServiceUnavailable, wantLevel: obslog.LevelError},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			app := fiber.New()
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Get("/status", func(c fiber.Ctx) error {
				return c.SendStatus(tt.status)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/status", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()
			require.Equal(t, tt.status, resp.StatusCode)

			levels := logger.levelSnapshot()
			require.Len(t, levels, 1)
			assert.Equal(t, tt.wantLevel, levels[0])
		})
	}
}

func TestHTTPMiddlewareTypedNilErrorsFailClosedWithoutPanicking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		handlerErr func() error
		wantStatus int
	}{
		{
			name: "direct typed-nil Fiber error",
			handlerErr: func() error {
				var err *fiber.Error

				return err
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "wrapped typed-nil Fiber error",
			handlerErr: func() error {
				var err *fiber.Error

				return staticWrappedError{err: err}
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "joined typed-nil Fiber error",
			handlerErr: func() error {
				var err *fiber.Error

				return errors.Join(err, errors.New("joined error"))
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "direct typed-nil custom error",
			handlerErr: func() error {
				var err *typedNilCustomError

				return err
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "wrapped typed-nil custom error",
			handlerErr: func() error {
				var err *typedNilCustomError

				return staticWrappedError{err: err}
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "joined typed-nil custom error",
			handlerErr: func() error {
				var err *typedNilCustomError

				return errors.Join(err, errors.New("joined error"))
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "ordinary Fiber error preserves status",
			handlerErr: func() error { return fiber.NewError(http.StatusTeapot) },
			wantStatus: http.StatusTeapot,
		},
		{
			name:       "ordinary error becomes internal server error",
			handlerErr: func() error { return errors.New("ordinary error") },
			wantStatus: http.StatusInternalServerError,
		},
		{
			// The regression this middleware exists to prevent: a valid,
			// correctly-mapped 4xx joined with an unrelated typed-nil error must
			// keep its original status. The old chain-walking normalizer treated
			// the typed-nil anywhere in the tree as disqualifying, collapsing this
			// to a 500 even though the top-level errors.Join value is not nil.
			name: "joined valid Fiber error with typed-nil sibling preserves original status",
			handlerErr: func() error {
				var nilErr *typedNilCustomError

				return errors.Join(fiber.NewError(http.StatusBadRequest, "bad input"), nilErr)
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tel, _, _ := newTelemetryHarness(t)
			logger := &captureLogger{}
			// A realistic ErrorHandler derives the status via errors.As (same
			// mechanism httpStatusCode uses internally) and responds with a fixed
			// body, exactly as domain.md's client-facing-error rule requires for a
			// 5xx. It deliberately never calls err.Error(): under the new
			// top-level-only contract, a valid errors.Join carrying an unrelated
			// typed-nil member (a case this suite constructs on purpose) panics
			// when stringified - that is a pre-existing stdlib gotcha in
			// errors.Join itself, not something this middleware can or should
			// paper over by walking the whole chain again.
			app := fiber.New(fiber.Config{
				ErrorHandler: func(c fiber.Ctx, err error) error {
					code := http.StatusInternalServerError

					var fiberErr *fiber.Error
					if errors.As(err, &fiberErr) && fiberErr != nil {
						code = fiberErr.Code
					}

					return c.Status(code).SendString("error")
				},
			})
			app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Get("/error", func(fiber.Ctx) error { return tt.handlerErr() })

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/error", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
		})
	}
}

func TestNormalizeHTTPHandlerErrorRejectsEveryTypedNilKind(t *testing.T) {
	t.Parallel()

	var (
		pointerErr *typedNilCustomError
		mapErr     typedNilMapError
		sliceErr   typedNilSliceError
		funcErr    typedNilFuncError
		channelErr typedNilChannelError
	)

	tests := []struct {
		name string
		err  error
	}{
		{name: "pointer", err: pointerErr},
		{name: "map", err: mapErr},
		{name: "slice", err: sliceErr},
		{name: "function", err: funcErr},
		{name: "channel", err: channelErr},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := normalizeHTTPHandlerError(tt.err).(*fiber.Error)
			require.True(t, ok)
			assert.Same(t, fiber.ErrInternalServerError, got)
		})
	}
}

// TestNormalizeHTTPHandlerErrorPassesThroughNestedTypedNil locks in the new
// contract: normalizeHTTPHandlerError checks ONLY the top-level value. A
// valid, non-nil wrapper or errors.Join result is returned unchanged even
// when its Unwrap chain bottoms out on a typed-nil somewhere below the top.
func TestNormalizeHTTPHandlerErrorPassesThroughNestedTypedNil(t *testing.T) {
	t.Parallel()

	var nilFiberErr *fiber.Error

	var nilCustomErr *typedNilCustomError

	tests := []struct {
		name    string
		err     error
		wantPtr bool // true: err is a pointer, compare by identity (assert.Same)
	}{
		{name: "wrapped typed-nil Fiber error", err: staticWrappedError{err: nilFiberErr}},
		{name: "wrapped typed-nil custom error", err: staticWrappedError{err: nilCustomErr}},
		{
			name:    "joined typed-nil custom error with a valid sibling",
			err:     errors.Join(errors.New("sibling"), nilCustomErr),
			wantPtr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalizeHTTPHandlerError(tt.err)

			if tt.wantPtr {
				assert.Same(t, tt.err, got,
					"a valid top-level error wrapping a nested typed-nil must pass through unchanged")

				return
			}

			// staticWrappedError is a value type: identity comparison does not
			// apply, so equality is the right check for "passed through unchanged".
			assert.Equal(t, tt.err, got,
				"a valid top-level error wrapping a nested typed-nil must pass through unchanged")
		})
	}
}

func TestHTTPMiddlewareTypedNilErrorsFailClosedOnBypassPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		path  string
		setup func(*fiber.App)
	}{
		{
			name: "nil telemetry bypass",
			path: "/error",
			setup: func(app *fiber.App) {
				app.Use(NewTelemetryMiddleware(nil).WithTelemetry(nil))
			},
		},
		{
			name: "excluded telemetry bypass",
			path: "/error",
			setup: func(app *fiber.App) {
				tel := &tracing.Telemetry{}
				app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel, "/error"))
			},
		},
		{
			name: "excluded logging bypass",
			path: "/error",
			setup: func(app *fiber.App) {
				app.Use(WithHTTPLogging(WithExcludedRoutes("/error")))
			},
		},
		{
			name: "Swagger logging bypass",
			path: "/swagger/openapi.json",
			setup: func(app *fiber.App) {
				app.Use(WithHTTPLogging())
			},
		},
		{
			name: "span-ending middleware",
			path: "/error",
			setup: func(app *fiber.App) {
				app.Use(NewTelemetryMiddleware(nil).EndTracingSpans)
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New(fiber.Config{
				ErrorHandler: func(c fiber.Ctx, err error) error {
					if err == fiber.ErrInternalServerError {
						return c.SendStatus(http.StatusInternalServerError)
					}

					return c.SendStatus(http.StatusTeapot)
				},
			})
			tt.setup(app)
			app.Get(tt.path, func(fiber.Ctx) error {
				var err *typedNilCustomError

				return err
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tt.path, nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		})
	}
}

func TestWithGrpcLoggingUsesBodyRequestIDAndLogsResult(t *testing.T) {
	logger := &captureLogger{}
	bodyRequestID := "4fbf011b-bb11-4c73-9f7c-4f8e19ca8402"
	metadataRequestID := "7cb344b8-b5ec-4bf7-a2a1-0320763bdc67"
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(metadataID, metadataRequestID))

	interceptor := WithGrpcLogging(WithCustomLogger(logger))
	resp, err := interceptor(
		ctx,
		grpcRequestWithID{RequestId: bodyRequestID},
		&grpc.UnaryServerInfo{FullMethod: "/service.Method"},
		func(ctx context.Context, _ any) (any, error) {
			_, _, requestID, _ := observability.NewTrackingFromContext(ctx)
			assert.Equal(t, bodyRequestID, requestID)
			assert.Same(t, logger, observability.NewLoggerFromContext(ctx))
			assert.Contains(t, observability.AttributesFromContext(ctx), attribute.String("app.request.request_id", bodyRequestID))

			return "ok", nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)

	messages, fields := logger.snapshot()
	assert.Contains(t, messages, "Overriding correlation id from metadata with body request_id")
	assert.Contains(t, messages, "gRPC request finished")
	assert.Contains(t, fields, obslog.String(headerID, bodyRequestID))
}

func TestWithGrpcLoggingLogsHandlerError(t *testing.T) {
	logger := &captureLogger{}
	expectedErr := errors.New("handler failed")

	interceptor := WithGrpcLogging(WithCustomLogger(logger))
	resp, err := interceptor(
		context.Background(),
		grpcRequestWithID{RequestId: "4fbf011b-bb11-4c73-9f7c-4f8e19ca8402"},
		&grpc.UnaryServerInfo{FullMethod: "/service.Method"},
		func(context.Context, any) (any, error) {
			return nil, expectedErr
		},
	)

	assert.Nil(t, resp)
	assert.ErrorIs(t, err, expectedErr)

	_, fields := logger.snapshot()
	assert.Contains(t, fields, obslog.Err(expectedErr))
}

// typedNilGRPCLoggingError has an unsafe Error() implementation (dereferences
// the nil receiver's field) so this test proves WithGrpcLogging survives a
// handler bug rather than by coincidence.
type typedNilGRPCLoggingError struct {
	message string
}

func (e *typedNilGRPCLoggingError) Error() string {
	return e.message
}

// TestWithGrpcLoggingHandlerReturnsTypedNilDoesNotPanic mirrors
// grpcmiddleware's TestNormalizeGRPCError_CatchesEveryUnsafeShape:
// WithGrpcLogging returns whatever the handler gives it straight to
// grpc-go's own dispatch, which calls status.FromError - and, on its
// fallback path, err.Error() unconditionally. A bare top-level typed-nil,
// and a valid non-nil errors.Join wrapping one, both used to panic there.
func TestWithGrpcLoggingHandlerReturnsTypedNilDoesNotPanic(t *testing.T) {
	t.Parallel()

	var typedNil *typedNilGRPCLoggingError

	tests := []struct {
		name string
		err  error
	}{
		{name: "bare top-level typed-nil", err: typedNil},
		{name: "errors.Join with a typed-nil member", err: errors.Join(errors.New("sibling"), typedNil)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			interceptor := WithGrpcLogging(WithCustomLogger(logger))

			var (
				resp any
				err  error
			)

			require.NotPanics(t, func() {
				resp, err = interceptor(
					context.Background(),
					"req",
					&grpc.UnaryServerInfo{FullMethod: "/service.Method"},
					func(context.Context, any) (any, error) { return nil, tt.err },
				)
			})

			assert.Nil(t, resp)
			require.Error(t, err)
			require.NotPanics(t, func() { _ = err.Error() },
				"the normalized error must itself be safe to stringify")
		})
	}
}

// TestWithHTTPLoggingResolvesRouteTemplateFromPreChainMiddleware guards the
// middleware-ordering contract: WithHTTPLogging constructs RequestInfo before
// the downstream chain runs, when Fiber's Route().Path still reports the
// middleware's own route ("/"). The logged path must be the final matched
// route template, never "/" and never the concrete path.
func TestWithHTTPLoggingResolvesRouteTemplateFromPreChainMiddleware(t *testing.T) {
	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/v1/orders/:id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/orders/12345", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	messages, fields := logger.snapshot()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "GET /v1/orders/:id")
	assert.NotContains(t, messages[0], "/v1/orders/12345")
	assert.Contains(t, fields, obslog.String("http_path", "/v1/orders/:id"))
}
