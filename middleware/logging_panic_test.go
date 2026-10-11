//go:build unit

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	obslog "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Services mount recover OUTSIDE the logger, so a panic unwinds through it.
func TestWithHTTPLoggingLogsPanicAsServerErrorAndRepanics(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}

	var outerRecovered any

	app := fiber.New()
	app.Use(recover.New(recover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: func(_ fiber.Ctx, recovered any) { outerRecovered = recovered },
	}))
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/boom", func(fiber.Ctx) error { panic("boom") })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "boom", outerRecovered, "the panic must still reach the outer recover")

	messages, fields := logger.snapshot()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], `"GET /boom" 500 -`)
	assert.Equal(t, []int{obslog.LevelError}, logger.levelSnapshot())
	assert.Contains(t, fields, obslog.String("error", "panic: boom"))
	assert.Contains(t, fields, obslog.Int("http_status_code", http.StatusInternalServerError))
}
