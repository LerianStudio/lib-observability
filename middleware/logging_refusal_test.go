//go:build unit

package middleware

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	obslog "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// refusalApp mirrors the services' stack: recover outside the logger, the
// finalizer inside it, and an ErrorHandler rendering the Fiber error's message
// as lib-commons' FiberErrorHandler does.
func refusalApp(logger *captureLogger) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit: 64,
		ErrorHandler: func(c fiber.Ctx, err error) error {
			fiberErr := asFiberError(err)
			if fiberErr == nil {
				fiberErr = fiber.ErrInternalServerError
			}

			return c.Status(fiberErr.Code).JSON(fiber.Map{"code": fiberErr.Code, "title": "Request Failed", "message": fiberErr.Message})
		},
	})
	LogRefusalsBeforeRouting(app, WithCustomLogger(logger))
	app.Use(recover.New())
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(WithHTTPErrorHandling())
	app.Post("/contracts", func(c fiber.Ctx) error { return c.SendString("ok") })

	return app
}

// serveOnLoopback serves app on a real loopback socket until the test ends.
func serveOnLoopback(t *testing.T, app *fiber.App) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	served := make(chan error, 1)
	go func() { served <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		if err := <-served; err != nil && !errors.Is(err, net.ErrClosed) {
			require.NoError(t, err)
		}
	})

	return ln.Addr().String()
}

func TestLogRefusalsBeforeRoutingLogsBodyLimitRefusalOnce(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}
	app := refusalApp(logger)

	req := httptest.NewRequest(http.MethodPost, "/contracts", strings.NewReader(strings.Repeat("a", 128)))
	_, err := app.Test(req)
	// fasthttp closes the connection after a pre-routing refusal; app.Test reports it.
	require.ErrorIs(t, err, fasthttp.ErrBodyTooLarge)

	messages, fields := logger.snapshot()
	require.Len(t, messages, 1, "one access line, not WithHTTPLogging's 200 plus the hook's 413")
	assert.Contains(t, messages[0], `"POST /contracts" 413`)
	assert.Contains(t, fields, obslog.String("http_path", "/contracts"), "the raw path the client called")
	assert.Equal(t, []int{obslog.LevelWarn}, logger.levelSnapshot())
	assert.Contains(t, fields, obslog.Int("http_status_code", http.StatusRequestEntityTooLarge))
	assert.Contains(t, fields, obslog.String("http_method", http.MethodPost))
	assert.Contains(t, fields, obslog.String("error", fasthttp.ErrBodyTooLarge.Error()))
	assert.Contains(t, fields, obslog.String("problem_title", "Request Failed"))
	assert.Contains(t, fields, obslog.String("problem_detail", "Request Entity Too Large"))
}

func TestLogRefusalsBeforeRoutingLeavesRoutedRequestsToWithHTTPLogging(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}
	app := refusalApp(logger)

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/contracts", strings.NewReader("small")))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	messages, _ := logger.snapshot()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], `"POST /contracts" 200`)
}

// A request fasthttp cannot read, or resets after reading (a chunked body), is
// refused with no headers left: the refusal is still logged, with no method,
// path or protocol to name, and the client's bytes in neither its cause nor its problem.
func TestLogRefusalsBeforeRoutingLogsUnreadableRequest(t *testing.T) {
	t.Parallel()

	const head = "POST /contracts HTTP/1.1\r\nHost: gw\r\nTransfer-Encoding: chunked\r\n\r\n"

	noHost := "POST /v1/dict/keys/maria@example.com HTTP/1.1\r\n\r\n"
	part := "--B\r\nMaria Silva maria@example.com\r\n\r\nv\r\n--B--\r\n"

	tests := []struct {
		name, request, wantError string
		wantStatus               int
	}{
		{"chunked body over the limit", head + "80\r\n" + strings.Repeat("a", 128) + "\r\n0\r\n\r\n", fasthttp.ErrBodyTooLarge.Error(), http.StatusRequestEntityTooLarge},
		{"malformed chunk", head + "zz\r\n", "empty hex number", http.StatusBadRequest},
		{"head without host", noHost, fmt.Sprintf("error when reading request headers: missing required host header in request: buffer size=%d", len(noHost)), http.StatusBadRequest},
		{"unencoded space in the query", "GET /contracts?name=Maria Silva&email=maria@example.com HTTP/1.1\r\nHost: gw\r\n\r\n", "error when reading request headers: unsupported http version", http.StatusBadRequest},
		{"malformed multipart part", fmt.Sprintf("POST /contracts HTTP/1.1\r\nHost: gw\r\nContent-Type: multipart/form-data; boundary=B\r\nContent-Length: %d\r\n\r\n%s", len(part), part), "cannot read multipart/form-data body: malformed MIME header: missing colon", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			app := refusalApp(logger)

			conn, err := net.Dial("tcp", serveOnLoopback(t, app))
			require.NoError(t, err)
			defer func() { require.NoError(t, conn.Close()) }()

			_, err = io.WriteString(conn, tt.request)
			require.NoError(t, err)

			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, tt.wantStatus, resp.StatusCode)

			messages, fields := logger.snapshot()
			require.Len(t, messages, 1)
			assert.Contains(t, messages[0], `127.0.0.1 - - - [`)
			assert.Contains(t, messages[0], fmt.Sprintf(`"- -" %d`, tt.wantStatus))
			assert.Equal(t, []int{obslog.LevelWarn}, logger.levelSnapshot())
			assert.Contains(t, fields, obslog.Int("http_status_code", tt.wantStatus))
			assert.Contains(t, fields, obslog.String("http_method", "-"))
			assert.Contains(t, fields, obslog.String("http_path", "-"))
			assert.Contains(t, fields, obslog.String("error", tt.wantError))

			for _, field := range fields {
				if field.Key == "error" || strings.HasPrefix(field.Key, "problem_") {
					assert.NotContains(t, fmt.Sprint(field.Value), "Silva", field.Key)
					assert.NotContains(t, fmt.Sprint(field.Value), "maria@example.com", field.Key)
				}
			}
		})
	}
}

// A connection reset before its first byte (a TCP health check, a scan) reaches
// the server error handler as a bare read error; no request to log.
func TestLogRefusalsBeforeRoutingSkipsConnectionsWithNoRequest(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}
	app := refusalApp(logger)

	accepted, handled := make(chan struct{}, 1), make(chan error, 1)
	app.Server().ConnState = func(_ net.Conn, state fasthttp.ConnState) {
		if state == fasthttp.StateNew {
			accepted <- struct{}{}
		}
	}

	hook := app.Server().ErrorHandler
	app.Server().ErrorHandler = func(fctx *fasthttp.RequestCtx, err error) {
		hook(fctx, err)
		handled <- err
	}

	addr, err := net.ResolveTCPAddr("tcp", serveOnLoopback(t, app))
	require.NoError(t, err)

	conn, err := net.DialTCP("tcp", nil, addr)
	require.NoError(t, err)

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never accepted the connection")
	}

	require.NoError(t, conn.SetLinger(0)) // close with a reset, not a FIN
	require.NoError(t, conn.Close())

	select {
	case err := <-handled:
		var opErr *net.OpError
		require.ErrorAs(t, err, &opErr)
	case <-time.After(5 * time.Second):
		t.Fatal("the reset never reached the server error handler")
	}

	messages, _ := logger.snapshot()
	assert.Empty(t, messages)
}

// A global middleware (a rate limiter mounted with Use) refuses before any
// route matches: the line names the endpoint by its shape, values as "*".
func TestWithHTTPLoggingNamesPathShapeOfRefusalBeforeAnyRoute(t *testing.T) {
	t.Parallel()

	tests := []struct{ target, wantPath string }{
		{"/v1/orders/12345?page=2", "/v1/orders/*"},
		{"/v1/dict/keys/holder@example.com", "/v1/dict/keys/*"},
		{"/reset/Zx9qT2rLmW8vK3pN", "/reset/*"},
		{"/reset/secret", "/reset/*"},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			app := fiber.New()
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Use(func(fiber.Ctx) error { return fiber.ErrTooManyRequests })

			ok := func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) }
			app.Get("/v1/orders/:id", ok)
			app.Get("/v1/dict/keys/:key", ok)
			app.Post("/reset/:token", ok)

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tt.target, nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

			messages, fields := logger.snapshot()
			require.Len(t, messages, 1)
			assert.Contains(t, messages[0], `"GET `+tt.wantPath+`" 429`)
			assert.Contains(t, fields, obslog.String("http_path", tt.wantPath))
		})
	}
}
