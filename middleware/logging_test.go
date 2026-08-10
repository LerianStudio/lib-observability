//go:build unit

package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	observability "github.com/LerianStudio/lib-observability/v2"
	obslog "github.com/LerianStudio/lib-observability/v2/log"
	"github.com/gofiber/fiber/v3"
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
}

func (l *captureLogger) Log(_ context.Context, _ obslog.Level, msg string, fields ...obslog.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.messages = append(l.messages, msg)
	l.fields = append(l.fields, fields...)
}

func (l *captureLogger) With(fields ...obslog.Field) obslog.Logger {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.fields = append(l.fields, fields...)

	return l
}

func (l *captureLogger) WithGroup(_ string) obslog.Logger { return l }

func (l *captureLogger) Enabled(_ obslog.Level) bool { return true }

func (l *captureLogger) Sync(_ context.Context) error { return nil }

func (l *captureLogger) snapshot() ([]string, []obslog.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()

	messages := append([]string(nil), l.messages...)
	fields := append([]obslog.Field(nil), l.fields...)

	return messages, fields
}

type grpcRequestWithID struct {
	RequestId string
}

func (r grpcRequestWithID) GetRequestId() string {
	return r.RequestId
}

// TestNewRequestInfoNeverCapturesRequestBody guards the fix for a body-capture
// bug that broke multi-GB streaming uploads: NewRequestInfo used to call
// c.Body() unconditionally, which consumes/buffers the request body before
// the downstream handler can read it as a stream (RequestBodyStream() then
// returns nil). Access logging must never touch the body at all; Referer and
// Username are fixed "-" placeholders rather than parsed from the request.
func TestNewRequestInfoNeverCapturesRequestBody(t *testing.T) {
	app := fiber.New()
	app.Post("/charge", func(c fiber.Ctx) error {
		info := NewRequestInfo(c, false)

		assert.Equal(t, "POST", info.Method)
		assert.Empty(t, info.URI, "URI must stay unresolved until FinishRequestInfo runs after routing")
		assert.Equal(t, "-", info.Referer)
		assert.Equal(t, "-", info.Username)
		assert.Equal(t, "agent", info.UserAgent)
		assert.Empty(t, info.Body, "request bodies are never captured by access logging")

		// The downstream handler must still be able to read the full body
		// afterward: proves NewRequestInfo did not consume or buffer it.
		assert.Equal(t, "{\"password\":\"secret\"}", string(c.Body()))

		info.FinishRequestInfo(&ResponseMetricsWrapper{Context: c, StatusCode: http.StatusNoContent})
		assert.Equal(t, "/charge", info.URI)

		return c.SendStatus(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/charge?name=alice&password=secret", strings.NewReader("{\"password\":\"secret\"}"))
	req.Header.Set(headerReferer, "https://user:pass@example.com/path?token=secret#fragment")
	req.Header.Set(headerUserAgent, "agent")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
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

	messages, _ := logger.snapshot()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "GET /fail")
	assert.Contains(t, messages[0], " 500 ")
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
