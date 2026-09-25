package runtime

import (
	"context"
	"fmt"
	"regexp"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/LerianStudio/lib-observability/v4/internal/panicobs"
)

// maxPanicValueLen is the maximum length for a panic value string exported to spans.
const maxPanicValueLen = 1024

// sensitivePattern matches common sensitive data patterns for redaction in span attributes.
// Covers passwords, tokens, secrets, API keys, credentials, and connection strings,
// plus the credential after every Bearer or Basic scheme word, with or without a key.
var sensitivePattern = regexp.MustCompile(
	`(?i)(password|passwd|pwd|token|secret|api[_-]?key|credential|bearer|authorization)[=:]\s*(?:(?:bearer|basic)\s+)?\S+|\b(?:bearer|basic)\s+\S+`,
)

// sensitiveRedaction is the replacement string for redacted sensitive data.
const sensitiveRedaction = "[REDACTED]"

// sanitizePanicValue truncates and redacts sensitive patterns from a panic value string.
func sanitizePanicValue(raw string) string {
	sanitized := sensitivePattern.ReplaceAllString(raw, sensitiveRedaction)

	if len(sanitized) > maxPanicValueLen {
		return sanitized[:maxPanicValueLen] + "...[truncated]"
	}

	return sanitized
}

// ErrPanic is the sentinel error for recovered panics recorded to spans.
var ErrPanic = panicobs.ErrPanic

// PanicSpanEventName is the event name used when recording panic events on spans.
const PanicSpanEventName = constant.EventPanicRecovered

// RecordPanicToSpan records a recovered panic as an error event on the current span.
// This enriches distributed traces with panic information for debugging.
//
// The function:
//   - Adds a "panic.recovered" event with panic value, stack trace, and goroutine name
//   - Records the panic as an error using span.RecordError
//   - Sets the span status to Error with a descriptive message
//
// Parameters:
//   - ctx: Context containing the active span
//   - panicValue: The value passed to panic()
//   - stack: The stack trace captured via debug.Stack()
//   - goroutineName: The name of the goroutine where the panic occurred
//
// If there is no active span in the context, this function is a no-op.
func RecordPanicToSpan(ctx context.Context, panicValue any, stack []byte, goroutineName string) {
	recordPanicToSpanInternal(ctx, panicValue, stack, "", goroutineName)
}

// RecordPanicToSpanWithComponent is like RecordPanicToSpan but also includes the component name.
// This is useful for HTTP/gRPC handlers where both component and handler name are relevant.
//
// Parameters:
//   - ctx: Context containing the active span
//   - panicValue: The value passed to panic()
//   - stack: The stack trace captured via debug.Stack()
//   - component: The service component (e.g., "transaction", "onboarding")
//   - goroutineName: The name of the handler or goroutine
func RecordPanicToSpanWithComponent(
	ctx context.Context,
	panicValue any,
	stack []byte,
	component, goroutineName string,
) {
	recordPanicToSpanInternal(ctx, panicValue, stack, component, goroutineName)
}

// recordPanicToSpanInternal is the shared implementation for recording panic events.
// Panic values are sanitized to prevent leaking sensitive data into
// distributed tracing backends; panicobs truncates the stack.
func recordPanicToSpanInternal(
	ctx context.Context,
	panicValue any,
	stack []byte,
	component, goroutineName string,
) {
	panicobs.RecordToSpan(ctx, sanitizePanicValue(fmt.Sprintf("%v", panicValue)), stack, component, goroutineName)
}
