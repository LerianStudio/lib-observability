package log

import (
	"context"
	"fmt"
)

// SafeError logs errors with explicit production-aware sanitization.
//
// logger is typed Universal, so a caller can pass a logger declared in its own
// package without importing this one; any Logger satisfies it unchanged.
//
// When production is true, only the error type is logged (no message details).
//
// Design rationale: the production boolean is caller-supplied rather than
// derived from a global flag. This keeps the log package free of global state
// and lets the caller (typically a service boundary) decide the sanitization
// policy based on its own configuration. Callers in production deployments
// should pass true to prevent leaking sensitive error details into log output.
func SafeError(logger Universal, ctx context.Context, msg string, err error, production bool) {
	if IsNil(logger) {
		return
	}

	if IsNil(err) {
		return
	}

	// Adapt supplies Enabled for a Log-only logger; a real Logger is returned
	// unchanged, so its own level check still short-circuits here.
	adapted := Adapt(logger)
	if !adapted.Enabled(LevelError) {
		return
	}

	if production {
		adapted.Log(ctx, LevelError, msg, String("error_type", fmt.Sprintf("%T", err)))
		return
	}

	adapted.Log(ctx, LevelError, msg, Err(err))
}

// SanitizeExternalResponse removes potentially sensitive external response data.
// Returns only status code for error messages.
func SanitizeExternalResponse(statusCode int) string {
	return fmt.Sprintf("external system returned status %d", statusCode)
}
