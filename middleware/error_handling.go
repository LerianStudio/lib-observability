package middleware

import (
	"context"
	"errors"
	"fmt"

	observability "github.com/LerianStudio/lib-observability/v3"
	obslog "github.com/LerianStudio/lib-observability/v3/log"
	"github.com/LerianStudio/lib-observability/v3/runtime"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/gofiber/fiber/v3"
)

type httpResponseStateKey struct{}

type httpResponseState struct {
	originalErr error
	statusCode  int
	finalized   bool
}

// WithHTTPErrorHandling finalizes Fiber handler errors before outer logging and
// telemetry middleware inspect the response.
//
// Registering it is a CORRECTNESS requirement, not a recommendation: without
// it, a valid, non-nil handler error whose Unwrap chain hits a typed-nil
// (errors.Join(fiber.NewError(400, ...), typedNil) is the reproduced case)
// reaches Fiber's own default ErrorHandler, which calls err.Error()
// unconditionally and panics - underneath any panic-recovery middleware
// registered ABOVE this one in the chain, since recovery only catches a
// panic in code it wraps, and Fiber's own dispatch invokes the ErrorHandler
// after the wrapped chain has already returned. Register it after
// WithHTTPLogging and WithTelemetry, and before application middleware and
// routes. It invokes the request's effective Fiber error handler exactly
// once, applies Fiber's 500 fallback when that handler fails, and returns
// nil so Fiber does not render the same error a second time.
//
// Two ordering constraints this middleware depends on, both MANDATORY:
//
//  1. No middleware that can return a non-nil error AFTER calling c.Next() may sit
//     between the observability middleware (WithHTTPLogging / WithTelemetry) and
//     this one. This middleware always returns nil, so under correct ordering
//     nothing reaches Fiber's own top-level dispatch with a non-nil error. A
//     middleware placed between them that manufactures its own post-c.Next()
//     error bypasses that: Fiber's dispatcher then invokes the app-level
//     ErrorHandler a second time on whatever it returned, in addition to the
//     invocation this middleware already performed.
//  2. Panic recovery (Fiber's built-in recover middleware, or an equivalent) must
//     be present SOMEWHERE in the chain. It is safe on either side of this
//     middleware - above it protects logging/telemetry as well, below it
//     protects only the application handler - but at least one instance is
//     required: this middleware normalizes returned errors, it does not recover
//     panics.
func WithHTTPErrorHandling() fiber.Handler {
	return func(c fiber.Ctx) error {
		state := &httpResponseState{}
		c.SetContext(context.WithValue(c.Context(), httpResponseStateKey{}, state))

		raw := c.Next()
		normalizedErr := normalizeHTTPHandlerError(raw)
		// Preserve the pre-normalization value for telemetry/logging: its true
		// type (via reflection) is more useful for debugging a handler bug than
		// the generic fiber.ErrInternalServerError it might later be substituted
		// with. A typed-nil raw is NOT a success: normalizeHTTPHandlerError maps
		// it to fiber.ErrInternalServerError and the ErrorHandler renders a 500,
		// so the normalized value is stored in that case to keep an error field
		// on the access log and error.type_original on the span of that 500.
		// The typed-nil raw itself is never stored: a typed-nil interface value
		// satisfies every handlerErr != nil check downstream but renders as
		// "<nil>", which names nothing an operator can act on.
		switch {
		case raw == nil:
			// Genuine success: no error to attribute.
		case obslog.IsNil(raw):
			state.originalErr = normalizedErr
		default:
			state.originalErr = raw
		}

		if normalizedErr != nil {
			// normalizeHTTPHandlerError only rejects a top-level typed-nil; a
			// valid, non-nil error whose Unwrap chain hits an unguarded
			// typed-nil (errors.Join(fiber.NewError(400, ...), typedNil) is
			// the reproduced case) passes through intact - by design, so its
			// mapped status survives errors.As-based extraction. But if the
			// app's configured ErrorHandler is (or falls back to) Fiber's own
			// DEFAULT one, it calls err.Error() unconditionally to render the
			// body and panics - invokeErrorHandlerSafely's recover then
			// downgrades that mapped 4xx to a generic 500. Substitute a
			// guaranteed-safe error before handing anything to the
			// ErrorHandler, so the code survives even when the chain isn't
			// stringifiable. errors.As never calls Error(), and *fiber.Error's
			// own Message field is a plain string, so when the chain DOES
			// contain one, both its status AND its original message survive
			// intact - only a chain with no *fiber.Error at all falls back to
			// a generic message (there is nothing safe to recover: the status
			// itself already came from httpStatusCode's own fallback).
			if !obslog.IsSafeToStringify(normalizedErr) {
				var fiberErr *fiber.Error
				if errors.As(normalizedErr, &fiberErr) {
					normalizedErr = fiber.NewError(fiberErr.Code, fiberErr.Message)
				} else {
					normalizedErr = fiber.NewError(httpStatusCode(c, normalizedErr), "internal error")
				}
			}

			invokeErrorHandlerSafely(c, normalizedErr)
		}

		state.statusCode = c.Response().StatusCode()
		state.finalized = true

		return nil
	}
}

// invokeErrorHandlerSafely calls the app's configured ErrorHandler for err,
// recovering from any panic inside it. This is unrelated to the typed-nil
// safety normalizeHTTPHandlerError already provides - a panic here can come
// from any bug in an arbitrary, user-supplied ErrorHandler. Unrecovered, it
// would unwind past this middleware's own remaining statements below (never
// setting state.statusCode/finalized) AND past the observability
// middleware's post-c.Next() code above it (WithTelemetry, WithHTTPLogging),
// so the request would produce no access log line at all and a span with no
// route/status attribution. Recovering here and falling back to a plain 500
// keeps the normal finalization path running regardless, so telemetry above
// still sees a complete picture.
func invokeErrorHandlerSafely(c fiber.Ctx, err error) {
	defer func() {
		if r := recover(); r != nil {
			// A panic in a user-supplied ErrorHandler must never become an
			// invisible 500: this library IS the observability path, so
			// silently discarding the panic value here - the way a bare
			// `recover() != nil` check does - would mean the one component
			// meant to explain a failure produced none for its own failure.
			// HandlePanicValue logs it (with stack trace) AND records it on
			// the request's active span (still reachable from c.Context() -
			// the same span WithTelemetry started earlier in the chain).
			logger := observability.NewLoggerFromContext(c.Context())
			runtime.HandlePanicValue(c.Context(), logger, r, "middleware", "http_error_handler")
			forceInternalServerError(c)
		}
	}()

	if handlerErr := c.App().ErrorHandler(c, err); handlerErr != nil {
		forceInternalServerError(c)
	}
}

func forceInternalServerError(c fiber.Ctx) {
	if sendErr := c.SendStatus(fiber.StatusInternalServerError); sendErr != nil {
		c.Response().SetStatusCode(fiber.StatusInternalServerError)
	}
}

func httpResponseStateFromContext(ctx context.Context) *httpResponseState {
	if ctx == nil {
		return nil
	}

	state, _ := ctx.Value(httpResponseStateKey{}).(*httpResponseState)

	return state
}

// resolveHTTPResponse resolves the (status code, error to return, error
// for telemetry) triple for a completed request. Every middleware that
// inspects the outcome of c.Next() on the HTTP path (WithHTTPLogging,
// WithTelemetry) must call this instead of deriving the values itself, so
// they observe the SAME resolved outcome regardless of whether
// WithHTTPErrorHandling already finalized the response further down the
// chain.
func resolveHTTPResponse(c fiber.Ctx, returnedErr error) (statusCode int, chainErr, handlerErr error) {
	if state := httpResponseStateFromContext(c.Context()); state != nil && state.finalized {
		// The response is already fully decided: WithHTTPErrorHandling either
		// ran the app's ErrorHandler or the original handler succeeded, and the
		// response was written either way. returnedErr here is whatever a
		// middleware ABOVE the finalizer chose to return after its own
		// c.Next() - under the package's ordering contract that middleware
		// shouldn't exist, but a violation must not corrupt an already-decided
		// response. Returning it non-nil, even normalized so it can't panic,
		// still lets it bubble to Fiber's own top-level dispatch, which invokes
		// the app ErrorHandler a SECOND time on it - overwriting an
		// already-correct mapped status with a generic 500, or (on an
		// otherwise-successful request) changing what the client receives
		// while telemetry keeps reporting the pre-rogue status. Returning nil
		// is what prevents that; state.originalErr/state.statusCode still
		// carry the real, already-decided outcome for telemetry.
		if returnedErr != nil {
			diagnoseDiscardedRogueError(c, returnedErr)
		}

		return state.statusCode, nil, state.originalErr
	}

	normalizedErr := normalizeHTTPHandlerError(returnedErr)

	return httpStatusCode(c, normalizedErr), normalizedErr, normalizedErr
}

// diagnoseDiscardedRogueError logs the error a middleware above the
// finalizer returned in violation of the package's ordering contract (see
// WithHTTPErrorHandling's doc comment) - discarded from the response on
// purpose (see the finalized branch above), but a documented contract
// violation still needs to be diagnosable, not silently swallowed.
func diagnoseDiscardedRogueError(c fiber.Ctx, returnedErr error) {
	logger := observability.NewLoggerFromContext(c.Context())
	logger.Log(c.Context(), obslog.LevelWarn,
		"discarded an error a middleware returned above the already-finalized response",
		// error_type via %T (safe regardless of shape - reflect only, no
		// method call) because "error" alone renders "<nil>" for a typed-nil
		// rogue error, which names nothing an operator can act on to find the
		// offending middleware.
		obslog.String("error", tracing.ErrorMessage(returnedErr)),
		obslog.String("error_type", fmt.Sprintf("%T", returnedErr)))
}
