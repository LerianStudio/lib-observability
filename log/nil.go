package log

import (
	"context"
	"fmt"
)

// NopLogger is a no-op logger implementation.
type NopLogger struct{}

// NewNop creates a no-op logger implementation.
func NewNop() Logger {
	return &NopLogger{}
}

// IsNil reports whether value is nil or wraps a typed-nil value.
func IsNil(value any) bool {
	return value == nil || isTypedNil(value)
}

// safeErrorMessageFallback is returned by SafeErrorMessage when err's own
// Error() method cannot be called safely.
const safeErrorMessageFallback = "error message unavailable: panic calling Error()"

// SafeErrorMessage returns err.Error(), never panicking.
//
// Returns "<nil>" for an untyped or typed-nil err (see IsNil). Otherwise
// calls err.Error() under recover: a valid, non-nil err can still panic when
// stringified if its own Unwrap chain contains a typed-nil its Error()
// method does not guard - the standard library's errors.Join is exactly
// this, since it calls Error() on every joined member with no nil check, so
// errors.Join(validErr, typedNilErr) panics despite being a perfectly valid,
// non-nil top-level error.
//
// Every sink that stringifies an arbitrary error for logs, spans, or
// assertions should call this instead of err.Error() directly - the
// package's own code paths, plus tracing.ErrorMessage and assert.NoError,
// route through it for exactly that reason. Observability code must never be
// the reason a request panics, however the upstream error was assembled.
func SafeErrorMessage(err error) string {
	msg, _ := safeStringify(err)

	return msg
}

// IsSafeToStringify reports whether err's Error() method can be called
// without panicking (and is non-nil/non-typed-nil to begin with).
//
// Use this - not a shape check like IsNil alone - whenever a caller must
// PROVE an error is safe before handing it to third-party code with an
// unguarded Error() call, such as google.golang.org/grpc/status.FromError's
// fallback path. IsNil alone misses a valid, non-nil error whose Unwrap
// chain hits an unguarded typed-nil (errors.Join(real, typedNil)) or a
// custom wrapper that blindly delegates to a nil field - both are safe by
// IsNil's shape check and both panic on Error(). Proving stringifiability
// directly, rather than inspecting the error's shape, catches every form.
func IsSafeToStringify(err error) bool {
	_, safe := safeStringify(err)

	return safe
}

// safeStringify is the single recover point behind SafeErrorMessage and
// IsSafeToStringify.
func safeStringify(err error) (msg string, safe bool) {
	if IsNil(err) {
		return "<nil>", false
	}

	defer func() {
		if r := recover(); r != nil {
			// Carry the evidence in the returned message: this package cannot
			// log the panic without recursing into the logger, and the constant
			// alone names neither the failing error type nor the panic cause.
			// %T on err is reflect-only (no method call) and %v on r is
			// rendered by fmt, which recovers formatting panics itself - err's
			// Error() is never called again here, that call is what panicked.
			msg = fmt.Sprintf("%s (%T: %v)", safeErrorMessageFallback, err, r)
			safe = false
		}
	}()

	return err.Error(), true
}

// Log drops all log events.
func (l *NopLogger) Log(_ context.Context, _ int, _ string, _ ...any) {}

// With returns the same no-op logger.
//
//nolint:ireturn
func (l *NopLogger) With(_ ...any) Logger {
	return l
}

// WithGroup returns the same no-op logger.
//
//nolint:ireturn
func (l *NopLogger) WithGroup(_ string) Logger {
	return l
}

// Enabled always returns false for NopLogger.
func (l *NopLogger) Enabled(_ int) bool {
	return false
}

// Sync is a no-op and always returns nil.
func (l *NopLogger) Sync(_ context.Context) error { return nil }
