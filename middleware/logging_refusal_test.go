//go:build unit

package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	obslog "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// refusalApp mirrors the services' stack: recover outside the logger, the
// finalizer inside it, and an ErrorHandler rendering a problem body.
func refusalApp(logger *captureLogger) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit: 16,
		ErrorHandler: func(c fiber.Ctx, err error) error {
			fiberErr := asFiberError(err)
			if fiberErr == nil {
				fiberErr = fiber.ErrInternalServerError
			}

			c.Set(fiber.HeaderContentType, "application/problem+json")

			return c.Status(fiberErr.Code).SendString(`{"title":"` + fiberErr.Message + `","detail":"body over the limit"}`)
		},
	})
	LogRefusalsBeforeRouting(app, WithCustomLogger(logger))
	app.Use(recover.New())
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(WithHTTPErrorHandling())
	app.Post("/contracts", func(c fiber.Ctx) error { return c.SendString("ok") })

	return app
}

func TestLogRefusalsBeforeRoutingLogsBodyLimitRefusalOnce(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}
	app := refusalApp(logger)

	req := httptest.NewRequest(http.MethodPost, "/contracts", strings.NewReader(strings.Repeat("a", 64)))
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
	assert.Contains(t, fields, obslog.String("problem_title", "Request Entity Too Large"))
	assert.Contains(t, fields, obslog.String("problem_detail", "body over the limit"))
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

// A connection closed before one byte (a TCP health check, a scan) still
// reaches the server error handler; there is no request to log.
func TestLogRefusalsBeforeRoutingSkipsConnectionsWithNoRequest(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}
	app := refusalApp(logger)

	app.Server().ErrorHandler(&fasthttp.RequestCtx{}, io.ErrUnexpectedEOF)

	messages, _ := logger.snapshot()
	assert.Empty(t, messages)
}
