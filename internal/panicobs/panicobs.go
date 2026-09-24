// Package panicobs holds the recovered-panic reporting state and helpers that
// both runtime and log need.
//
// It exists because runtime imports log, so log cannot import runtime: the
// production-mode switch, the panic counter hook and the span recorder live
// here, below both. runtime's public API (SetProductionMode, InitPanicMetrics,
// RecordPanicToSpan, ErrPanic) is a thin layer over this package, so a panic
// recovered by either package is reported with identical semantics.
package panicobs

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrPanic is the sentinel error recorded on a span for a recovered panic.
var ErrPanic = errors.New("panic")

// maxStackTraceLen is the maximum length for a stack trace exported to spans.
const maxStackTraceLen = 4096

var productionMode atomic.Bool

// SetProductionMode switches stack traces and panic details out of reports.
func SetProductionMode(enabled bool) { productionMode.Store(enabled) }

// ProductionMode reports whether production mode is enabled.
func ProductionMode() bool { return productionMode.Load() }

// Counter increments the recovered-panic counter for a component and name.
type Counter func(ctx context.Context, component, name string)

var counter atomic.Pointer[Counter]

// SetCounter installs the function that records the recovered-panic counter.
// runtime installs its own when panic metrics are initialized; nil uninstalls.
func SetCounter(fn Counter) {
	if fn == nil {
		counter.Store(nil)

		return
	}

	counter.Store(&fn)
}

// Count records one recovered panic. It is a no-op until a Counter is set,
// which is exactly runtime's behaviour before InitPanicMetrics.
func Count(ctx context.Context, component, name string) {
	if fn := counter.Load(); fn != nil {
		(*fn)(ctx, component, name)
	}
}

// SanitizeStackTrace truncates a stack trace for safe span export.
func SanitizeStackTrace(stack []byte) string {
	s := string(stack)

	if len(s) > maxStackTraceLen {
		return s[:maxStackTraceLen] + "\n...[truncated]"
	}

	return s
}

// RecordToSpan records a recovered panic on the recording span in ctx: a
// panic.recovered event, an ErrPanic error and an Error status. value is the
// already-sanitized rendering the caller is willing to export. A nil ctx or a
// non-recording span makes it a no-op.
func RecordToSpan(ctx context.Context, value string, stack []byte, component, name string) {
	if ctx == nil {
		return
	}

	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("panic.value", value),
		attribute.String("panic.stack", SanitizeStackTrace(stack)),
		attribute.String("panic.goroutine_name", name),
	}

	if component != "" {
		attrs = append(attrs, attribute.String("panic.component", component))
	}

	span.AddEvent(constant.EventPanicRecovered, trace.WithAttributes(attrs...))
	span.RecordError(fmt.Errorf("%w: %s", ErrPanic, value))

	statusMsg := "panic recovered in " + name
	if component != "" {
		statusMsg = fmt.Sprintf("panic recovered in %s/%s", component, name)
	}

	span.SetStatus(codes.Error, statusMsg)
}
