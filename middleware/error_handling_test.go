//go:build unit

package middleware

import (
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type typedNilProbeError struct{ msg string }

func (e *typedNilProbeError) Error() string { return e.msg }

// A wrapper whose Unwrap delegates to a nil cause, so Error() panics when
// the chain is stringified but errors.As can still walk it.
type delegatingProbeError struct{ cause *typedNilProbeError }

func (e *delegatingProbeError) Error() string { return "wrapped: " + e.cause.Error() }
func (e *delegatingProbeError) Unwrap() error { return e.cause }

// Regression for the typed-nil *fiber.Error substitution panic: errors.As
// matches a typed-nil *fiber.Error inside a joined chain and used to leave
// fiberErr nil, so reading .Code panicked inside the finalizer - the very
// defect class WithHTTPErrorHandling exists to close.
func TestWithHTTPErrorHandling_UnsafeChains_NeverPanic(t *testing.T) {
	t.Parallel()

	typedNilFiber := (*fiber.Error)(nil)
	typedNilProbe := (*typedNilProbeError)(nil)

	tests := []struct {
		name       string
		handlerErr error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "joined typed-nil fiber errors only",
			handlerErr: errors.Join(error(typedNilFiber), error(typedNilFiber)),
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "internal error",
		},
		{
			name:       "typed-nil fiber error joined with real error",
			handlerErr: errors.Join(error(typedNilFiber), errors.New("boom")),
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "internal error",
		},
		{
			name:       "valid fiber error joined with typed-nil probe keeps code and message",
			handlerErr: errors.Join(fiber.NewError(fiber.StatusBadRequest, "bad request payload"), error(typedNilProbe)),
			wantStatus: fiber.StatusBadRequest,
			wantBody:   "bad request payload",
		},
		{
			name:       "delegating wrapper with nil cause and no fiber error",
			handlerErr: &delegatingProbeError{},
			wantStatus: fiber.StatusInternalServerError,
			wantBody:   "internal error",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Fiber's DEFAULT ErrorHandler calls err.Error() unconditionally,
			// which is exactly the render path the substitution protects.
			app := fiber.New()
			app.Use(WithHTTPErrorHandling())
			app.Get("/probe", func(fiber.Ctx) error { return tt.handlerErr })

			var resp *httptest.ResponseRecorder
			require.NotPanics(t, func() {
				req := httptest.NewRequest(fiber.MethodGet, "/probe", nil)
				res, err := app.Test(req)
				require.NoError(t, err)
				defer res.Body.Close()

				body, readErr := io.ReadAll(res.Body)
				require.NoError(t, readErr)

				resp = httptest.NewRecorder()
				resp.Code = res.StatusCode
				_, _ = resp.Body.Write(body)
			})

			assert.Equal(t, tt.wantStatus, resp.Code)
			assert.Equal(t, tt.wantBody, resp.Body.String())
		})
	}
}

// httpStatusCode is the second site that dereferenced a typed-nil
// *fiber.Error matched by errors.As.
func TestHTTPStatusCode_TypedNilFiberErrorInChain_DoesNotPanic(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	joined := errors.Join(error((*fiber.Error)(nil)), errors.New("boom"))

	app.Get("/probe", func(c fiber.Ctx) error {
		require.NotPanics(t, func() {
			status := httpStatusCode(c, joined)
			assert.Equal(t, fiber.StatusInternalServerError, status)
		})

		return nil
	})

	req := httptest.NewRequest(fiber.MethodGet, "/probe", nil)
	res, err := app.Test(req)
	require.NoError(t, err)
	defer res.Body.Close()
}
