//go:build unit

package middleware

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	obslog "github.com/LerianStudio/lib-observability/v3/log"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithHTTPErrorHandling_FinalizesErrorsExactlyOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		handlerErr  error
		configure   func(*fiber.Config, *atomic.Int32)
		prewrite    int
		wantStatus  int
		wantBody    string
		wantHandled int32
	}{
		{
			name:        "default handler",
			handlerErr:  errors.New("boom"),
			wantStatus:  http.StatusInternalServerError,
			wantBody:    "boom",
			wantHandled: 0,
		},
		{
			name:       "custom domain handler maps to not found",
			handlerErr: errors.New("domain missing"),
			configure: func(config *fiber.Config, calls *atomic.Int32) {
				config.ErrorHandler = func(c fiber.Ctx, err error) error {
					calls.Add(1)
					assert.Equal(t, "domain missing", err.Error())

					return c.Status(http.StatusNotFound).SendString("missing")
				}
			},
			wantStatus:  http.StatusNotFound,
			wantBody:    "missing",
			wantHandled: 1,
		},
		{
			name:       "prewritten client error and generic error finalizes as server error",
			handlerErr: errors.New("late failure"),
			prewrite:   http.StatusUnprocessableEntity,
			wantStatus: http.StatusInternalServerError,
			wantBody:   "late failure",
		},
		{
			name:       "configured handler failure uses Fiber fallback",
			handlerErr: errors.New("boom"),
			configure: func(config *fiber.Config, calls *atomic.Int32) {
				config.ErrorHandler = func(c fiber.Ctx, _ error) error {
					calls.Add(1)
					require.NoError(t, c.SendString("partial"))

					return errors.New("render failed")
				}
			},
			wantStatus:  http.StatusInternalServerError,
			wantBody:    "partial",
			wantHandled: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var handlerCalls atomic.Int32
			config := fiber.Config{}
			if tt.configure != nil {
				tt.configure(&config, &handlerCalls)
			}

			app := fiber.New(config)
			app.Use(WithHTTPErrorHandling())
			app.Get("/fail", func(c fiber.Ctx) error {
				if tt.prewrite != 0 {
					require.NoError(t, c.SendStatus(tt.prewrite))
				}

				return tt.handlerErr
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/fail", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, tt.wantBody, readResponseBody(t, resp))
			assert.Equal(t, tt.wantHandled, handlerCalls.Load())
		})
	}
}

func TestWithHTTPErrorHandling_UsesMountedApplicationErrorHandler(t *testing.T) {
	t.Parallel()

	var parentCalls atomic.Int32
	parent := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			parentCalls.Add(1)

			return c.Status(http.StatusInternalServerError).SendString("parent")
		},
	})
	parent.Use(WithHTTPErrorHandling())

	var childCalls atomic.Int32
	child := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			childCalls.Add(1)

			return c.Status(http.StatusNotFound).SendString("child")
		},
	})
	child.Get("/item", func(fiber.Ctx) error { return errors.New("missing") })
	parent.Use("/api", child)

	resp, err := parent.Test(httptest.NewRequest(http.MethodGet, "/api/item", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "child", readResponseBody(t, resp))
	assert.Zero(t, parentCalls.Load())
	assert.EqualValues(t, 1, childCalls.Load())
}

// TestWithHTTPErrorHandling_NormalizesTypedNilGraphs verifies the full
// three-way contract WithHTTPErrorHandling implements before handing an
// error to the app's ErrorHandler:
//   - a bare top-level typed-nil is replaced with the safe
//     fiber.ErrInternalServerError sentinel (normalizeHTTPHandlerError);
//   - a valid, non-nil top-level error that IS safely stringifiable (a %w
//     wrap is safe here - fmt's own panic recovery already resolved the
//     nested typed-nil to "<nil>" at construction time) passes through
//     unchanged, even with a typed-nil sibling deeper in its Unwrap chain;
//   - a valid, non-nil top-level error that is NOT safely stringifiable
//     (errors.Join with a typed-nil member: its Error() panics, since
//     errors.Join calls Error() on every member with no nil guard) is
//     replaced with a synthetic error preserving the ORIGINAL status code
//     (via errors.As, which never calls Error()) - and, when the chain
//     contains a *fiber.Error, its ORIGINAL message too (errors.As never
//     calls Error(), and *fiber.Error.Message is a plain string field, so
//     reading it is safe); a chain with no *fiber.Error at all has nothing
//     safe to recover beyond the httpStatusCode fallback (500), so it falls
//     back to a generic message - because handing the raw value to an
//     ErrorHandler that calls Error() (Fiber's own default one, in
//     particular) would panic and downgrade a correctly-mapped 4xx to a 500.
func TestWithHTTPErrorHandling_NormalizesTypedNilGraphs(t *testing.T) {
	t.Parallel()

	var typedNil *typedNilCustomError

	const (
		categorySentinel    = "sentinel"    // replaced with fiber.ErrInternalServerError
		categoryUnchanged   = "unchanged"   // passes through as the exact same object
		categorySubstituted = "substituted" // replaced, but the ORIGINAL status (and message, if a *fiber.Error was in the chain) is preserved
	)

	tests := []struct {
		name        string
		err         error
		category    string
		wantCode    int
		wantMessage string
	}{
		{
			name:     "direct top-level typed-nil is replaced with the safe sentinel",
			err:      typedNil,
			category: categorySentinel,
		},
		{
			name:     "wrapped typed-nil with a safely-stringifiable top-level error passes through intact",
			err:      fmt.Errorf("wrapped: %w", typedNil),
			category: categoryUnchanged,
		},
		{
			name:        "joined typed-nil with no *fiber.Error in chain falls back to a generic message",
			err:         errors.Join(errors.New("sibling"), typedNil),
			category:    categorySubstituted,
			wantCode:    http.StatusInternalServerError,
			wantMessage: "internal error",
		},
		{
			name:        "joined typed-nil with a *fiber.Error in chain preserves its status AND message",
			err:         errors.Join(fiber.NewError(http.StatusBadRequest, "bad request payload"), typedNil),
			category:    categorySubstituted,
			wantCode:    http.StatusBadRequest,
			wantMessage: "bad request payload",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				calls          atomic.Int32
				handlerErrSeen error
			)
			app := fiber.New(fiber.Config{
				ErrorHandler: func(c fiber.Ctx, err error) error {
					calls.Add(1)
					handlerErrSeen = err

					return c.SendStatus(http.StatusInternalServerError)
				},
			})
			app.Use(WithHTTPErrorHandling())
			app.Get("/fail", func(fiber.Ctx) error { return tt.err })

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/fail", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
			assert.EqualValues(t, 1, calls.Load())

			switch tt.category {
			case categorySentinel:
				assert.Same(t, fiber.ErrInternalServerError, handlerErrSeen)
			case categoryUnchanged:
				assert.Same(t, tt.err, handlerErrSeen,
					"a safely-stringifiable top-level error wrapping a nested typed-nil must reach the error handler unchanged")
			case categorySubstituted:
				assert.NotSame(t, tt.err, handlerErrSeen,
					"an unsafe-to-stringify error must never reach an ErrorHandler that might call Error() on it")

				fe, ok := handlerErrSeen.(*fiber.Error)
				require.True(t, ok, "the substitute must itself be a safe *fiber.Error")
				assert.Equal(t, tt.wantCode, fe.Code)
				assert.Equal(t, tt.wantMessage, fe.Message)
			}
		})
	}
}

// TestWithHTTPErrorHandling_DefaultFiberErrorHandlerPreservesMappedStatus
// exercises the case TestWithHTTPErrorHandling_NormalizesTypedNilGraphs
// cannot reach: Fiber's own DEFAULT ErrorHandler (no custom one configured),
// which unconditionally calls err.Error() to render the response body.
// Without the fix, errors.Join(fiber.NewError(400, ...), typedNil) panics
// there, invokeErrorHandlerSafely's recover downgrades it to a 500, and the
// 400 normalizeHTTPHandlerError worked to preserve is lost anyway. The
// substitution in WithHTTPErrorHandling keeps the 400 alive by never handing
// the unsafe-to-stringify value to the default handler in the first place -
// and, since errors.As never calls Error() and *fiber.Error.Message is a
// plain string, the ORIGINAL message survives too, not just the status.
func TestWithHTTPErrorHandling_DefaultFiberErrorHandlerPreservesMappedStatus(t *testing.T) {
	t.Parallel()

	var typedNil *typedNilCustomError

	app := fiber.New() // no custom ErrorHandler - Fiber's own default applies
	app.Use(WithHTTPErrorHandling())
	app.Get("/fail", func(fiber.Ctx) error {
		return errors.Join(fiber.NewError(http.StatusBadRequest, "bad request payload"), typedNil)
	})

	var resp *http.Response

	var err error

	require.NotPanics(t, func() {
		resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/fail", nil))
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"the mapped 400 must survive Fiber's default ErrorHandler, not be downgraded to 500")
	assert.Equal(t, "bad request payload", readResponseBody(t, resp),
		"the *fiber.Error's own message must survive too, not just its status")
}

// TestFinalizedResponseIsImmuneToRogueErrorAboveIt covers the finalized
// branch of resolveHTTPResponse against a middleware sitting between the
// observability middleware and WithHTTPErrorHandling (an ordering the
// package doc comment forbids, but which a truly finalized response must
// still survive) that manufactures its OWN error after c.Next(), unrelated
// to what the request already decided.
//
// Measured divergence this guards against: returning that rogue error
// (even normalized, so it can't panic) still let it bubble to Fiber's own
// top-level dispatch, which invokes the app's ErrorHandler a SECOND time -
// on a successful request, this turned an already-sent 200 into a 500 while
// logs/spans kept reporting 200 (the 5xx alert on that path never fires); on
// a request that mapped to 409, the second invocation overwrote it with a
// generic 500. The finalized branch now returns nil unconditionally: nothing
// above the finalizer can override an already-decided response, and
// state.originalErr/state.statusCode still carry the real outcome for
// telemetry.
func TestFinalizedResponseIsImmuneToRogueErrorAboveIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		routeErr    error
		wantStatus  int
		wantHandled int32
	}{
		{
			name:       "successful route survives a rogue error above the finalizer",
			wantStatus: http.StatusOK,
		},
		{
			name:        "mapped error route survives a rogue error above the finalizer",
			routeErr:    fiber.NewError(http.StatusConflict, "conflict"),
			wantStatus:  http.StatusConflict,
			wantHandled: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var handled atomic.Int32

			app := fiber.New(fiber.Config{
				ErrorHandler: func(c fiber.Ctx, err error) error {
					handled.Add(1)

					code := http.StatusInternalServerError

					var fe *fiber.Error
					if errors.As(err, &fe) {
						code = fe.Code
					}

					return c.Status(code).SendString("handled")
				},
			})

			app.Use(WithHTTPLogging())
			// A middleware violating the ordering contract: it sits between
			// logging and the finalizer, and returns its own error after
			// c.Next(), regardless of what the request already decided.
			app.Use(func(c fiber.Ctx) error {
				_ = c.Next()

				return errors.New("rogue error unrelated to this request's outcome")
			})
			app.Use(WithHTTPErrorHandling())
			app.Get("/thing", func(c fiber.Ctx) error {
				if tt.routeErr != nil {
					return tt.routeErr
				}

				return c.SendStatus(http.StatusOK)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/thing", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			assert.Equal(t, tt.wantStatus, resp.StatusCode,
				"the client must see the response the request actually decided, not the rogue error's")
			assert.EqualValues(t, tt.wantHandled, handled.Load(),
				"the app ErrorHandler must not run a second time for the rogue error")
		})
	}
}

// TestFinalizedResponse_DiagnosesDiscardedRogueError verifies that a rogue
// error discarded by the finalized branch (see
// TestFinalizedResponseIsImmuneToRogueErrorAboveIt) still reaches the
// request-scoped logger - a documented contract violation must be
// diagnosable, not silently swallowed just because the response itself is
// correctly immune to it.
func TestFinalizedResponse_DiagnosesDiscardedRogueError(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}

	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(func(c fiber.Ctx) error {
		_ = c.Next()

		return errors.New("rogue error unrelated to this request's outcome")
	})
	app.Use(WithHTTPErrorHandling())
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ok", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	messages, _ := logger.snapshot()

	var foundDiagnostic bool

	for _, msg := range messages {
		if strings.Contains(msg, "discarded an error") {
			foundDiagnostic = true
		}
	}

	assert.True(t, foundDiagnostic,
		"a rogue error discarded above the finalizer must still be diagnosable, not silently swallowed")
}

// TestFinalizedResponse_DiagnosedRogueErrorIncludesTypeForTypedNil covers the
// case tracing.ErrorMessage alone cannot identify: a typed-nil rogue error
// renders as the message "<nil>" (log.SafeErrorMessage's safe placeholder),
// which names nothing an operator can search for. The error_type field
// (reflect-based, safe regardless of shape) names the concrete type instead,
// so the offending middleware is still findable.
func TestFinalizedResponse_DiagnosedRogueErrorIncludesTypeForTypedNil(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}

	var typedNil *typedNilCustomError

	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(func(c fiber.Ctx) error {
		_ = c.Next()

		return typedNil
	})
	app.Use(WithHTTPErrorHandling())
	app.Get("/ok", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ok", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	_, fields := logger.snapshot()

	var errorType string

	for _, f := range fields {
		if f.Key == "error_type" {
			errorType, _ = f.Value.(string)
		}
	}

	assert.Equal(t, "*middleware.typedNilCustomError", errorType,
		"error_type must name the concrete type, since the message alone is just \"<nil>\" for a typed-nil rogue error")
}

func TestHTTPObservability_UsesFinalResponseInBothMiddlewareOrderings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wire func(*fiber.App, *TelemetryMiddleware, *captureLogger)
	}{
		{
			name: "logging then telemetry",
			wire: func(app *fiber.App, telemetry *TelemetryMiddleware, logger *captureLogger) {
				app.Use(WithHTTPLogging(WithCustomLogger(logger)))
				app.Use(telemetry.WithTelemetry(telemetry.Telemetry))
				app.Use(WithHTTPErrorHandling())
			},
		},
		{
			name: "telemetry then logging",
			wire: func(app *fiber.App, telemetry *TelemetryMiddleware, logger *captureLogger) {
				app.Use(telemetry.WithTelemetry(telemetry.Telemetry))
				app.Use(WithHTTPLogging(WithCustomLogger(logger)))
				app.Use(WithHTTPErrorHandling())
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tel, _, spans := newTelemetryHarness(t)
			telemetry := NewTelemetryMiddleware(tel)
			logger := &captureLogger{}
			var calls atomic.Int32
			app := fiber.New(fiber.Config{
				ErrorHandler: func(c fiber.Ctx, _ error) error {
					calls.Add(1)

					return c.Status(http.StatusNotFound).SendString("missing")
				},
			})
			tt.wire(app, telemetry, logger)
			app.Get("/items/:id", func(fiber.Ctx) error { return errors.New("missing") })

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/items/secret-id", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
			assert.EqualValues(t, 1, calls.Load())

			messages, _ := logger.snapshot()
			require.Len(t, messages, 1)
			assert.Contains(t, messages[0], "GET /items/:id")
			assert.Contains(t, messages[0], " 404 ")
			assert.NotContains(t, messages[0], "secret-id")

			recorded := spans.GetSpans()
			require.Len(t, recorded, 1)
			assert.Equal(t, "404", getSpanAttr(recorded[0], "http.response.status_code"))
		})
	}
}

// TestWithHTTPErrorHandling_RecoversPanicInsideAppErrorHandler covers a bug
// unrelated to typed-nil safety: a panic inside the app's OWN, user-supplied
// ErrorHandler. Unrecovered, that panic unwinds past this middleware's
// remaining statements (state.statusCode/finalized never set) and past the
// observability middleware's post-c.Next() code above it, so the request
// produces no access log line and a span with no route/status attribution at
// all (measured). Recovering locally and falling back to a plain 500 keeps
// the rest of the chain's finalization intact - and the panic itself must be
// LOGGED and RECORDED on the span, not silently discarded: this library is
// the observability path, so a bare `recover() != nil` that never surfaces
// the panic value would mean the one component meant to explain a failure
// produced none for its own failure.
func TestWithHTTPErrorHandling_RecoversPanicInsideAppErrorHandler(t *testing.T) {
	t.Parallel()

	tel, _, spans := newTelemetryHarness(t)
	logger := &captureLogger{}

	app := fiber.New(fiber.Config{
		ErrorHandler: func(fiber.Ctx, error) error {
			panic("bug in a user-supplied ErrorHandler")
		},
	})
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(NewTelemetryMiddleware(tel).WithTelemetry(tel))
	app.Use(WithHTTPErrorHandling())
	app.Get("/fail", func(fiber.Ctx) error { return errors.New("boom") })

	var resp *http.Response

	var err error

	require.NotPanics(t, func() {
		resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/fail", nil))
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// Two log lines are now expected: the panic itself (logged by
	// runtime.HandlePanicValue, see below) and the access log entry that
	// must still be written despite it.
	messages, fields := logger.snapshot()
	require.Len(t, messages, 2)

	var foundAccessLogLine bool

	for _, msg := range messages {
		if strings.Contains(msg, "GET /fail") && strings.Contains(msg, " 500 ") {
			foundAccessLogLine = true
		}
	}

	assert.True(t, foundAccessLogLine, "the access log must still be written despite the panic in the app ErrorHandler")

	var loggedPanic bool

	for _, f := range fields {
		if f.Key == "value" && f.Value == "bug in a user-supplied ErrorHandler" {
			loggedPanic = true
		}
	}

	assert.True(t, loggedPanic, "the panic value itself must reach the logger, not just trigger a generic 500")

	recorded := spans.GetSpans()
	require.Len(t, recorded, 1, "the span must still be finalized with route/status despite the panic")
	assert.Equal(t, "500", getSpanAttr(recorded[0], "http.response.status_code"))
	assert.Equal(t, "GET /fail", recorded[0].Name)

	var foundPanicEvent bool

	for _, event := range recorded[0].Events {
		if event.Name == "panic.recovered" {
			foundPanicEvent = true
			break
		}
	}

	assert.True(t, foundPanicEvent, "the panic must be recorded as a span event, not silently swallowed")
}

func TestWithHTTPLogging_WithoutFinalizerClassifiesGenericErrorAs500(t *testing.T) {
	t.Parallel()

	logger := &captureLogger{}
	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/fail", func(c fiber.Ctx) error {
		require.NoError(t, c.SendStatus(http.StatusUnprocessableEntity))

		return errors.New("late failure")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/fail", nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	messages, _ := logger.snapshot()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], " 500 ")
	levels := logger.levelSnapshot()
	require.Len(t, levels, 1)
	assert.Equal(t, obslog.LevelError, levels[0])
}

func TestNewRequestInfo_CompatibilityFlagsAreSecureNoOps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		flag bool
	}{
		{name: "obfuscation enabled compatibility value", flag: false},
		{name: "obfuscation disabled compatibility value", flag: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Post("/charge", func(c fiber.Ctx) error {
				info := NewRequestInfo(c, tt.flag)
				assert.Empty(t, info.Body)
				assert.Equal(t, "-", info.Referer)
				assert.Equal(t, "-", info.Username)

				return c.SendStatus(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodPost, "/charge?token=secret", strings.NewReader("secret-body"))
			req.Header.Set(headerReferer, "https://user:pass@example.com/private?token=secret#fragment")
			resp, err := app.Test(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		})
	}

	opts := buildOpts(WithObfuscationDisabled(true))
	require.NotNil(t, opts)
}

func readResponseBody(t *testing.T, resp *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return string(body)
}
