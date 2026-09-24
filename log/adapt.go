package log

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/LerianStudio/lib-observability/v4/internal/panicobs"
)

// Universal is the smallest thing this package is willing to call a logger:
// one method, built entirely from universal types.
//
// It is the type that appears in the PARAMETER position of every exported
// function in this module that accepts a logger. That is the whole point.
// A parameter typed Logger would still couple the caller to this module,
// even though Logger's own signatures are now universal, because Logger has
// self-returning methods (With(...) Logger) and a self-returning method
// cannot be declared by a foreign package - it has no way to name the return
// type. Universal has no such method, so a caller can declare
//
//	type myLogger interface {
//		Log(ctx context.Context, level int, msg string, fields ...any)
//	}
//
// in its own package, import nothing from lib-observability, and still hand
// its logger to this module. Conversion happens here, at the call, via Adapt.
//
// level is on this package's scale: Error=0, Warn=1, Info=2, Debug=3.
type Universal interface {
	Log(ctx context.Context, level int, msg string, fields ...any)
}

// Adapt lifts any Universal logger into a full Logger.
//
// It is the "convert at the call site" half of the universal-parameter
// design: exported functions accept Universal, then Adapt once, internally.
//
// Adapt never wraps unnecessarily:
//
//   - a value that already implements Logger (GoLogger, the zap adapter,
//     NopLogger, a consumer's own full implementation) is returned as-is, so
//     its native With/WithGroup/Enabled/Sync semantics - redaction, grouping,
//     encoder behavior, level checks - are preserved exactly;
//   - a nil or typed-nil value returns NewNop, so the result is always safe
//     to call;
//   - anything else is wrapped in a shim that supplies the four missing
//     methods.
//
// Shim semantics, for the wrapped case only: With and WithGroup bind fields
// and a dot-joined group path that are replayed ahead of the caller's own on
// every Log, since a Log-only logger has nowhere to store them. Enabled
// delegates to the wrapped logger's own Enabled(level int) bool when it has
// one; otherwise it reports true for any defined level, since answering false
// would silently drop entries. An undefined level always reports false. Sync
// is a no-op.
//
// The shim is also a panic boundary. A panic inside the wrapped logger's Log
// or Enabled - the only shim methods that run its code - never unwinds the
// caller: the log line is dropped (Enabled answers true, as it would with no
// level check) and the panic is reported the way runtime reports a recovered
// panic: one ERROR line on this package's stdlib logger naming the method and
// the panic value's type, never the value, message or fields, and one
// increment of panic_recovered_total (component "log", named by the method)
// once runtime.InitPanicMetrics is wired, and, for Log, a panic.recovered
// event on the recording span in ctx, valued by the panic's type. A value that
// already implements Logger is returned as-is and so is not guarded; wrap it
// with Guard to get the same net.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func Adapt(u Universal) Logger {
	if IsNil(u) {
		return NewNop()
	}

	if full, ok := u.(Logger); ok {
		return full
	}

	return &universalShim{next: u}
}

// reportLoggerPanic reports a value recovered from an adapted logger's method
// and whether there was one. The report names the method and the panic
// value's type only: the value, the message and the fields may all carry data
// the consumer redacts. It goes to panicFallback, never to the logger that
// panicked, counts on runtime's recovered-panic counter, and lands on the
// recording span in ctx, if any. Reporting is best effort: a panic inside it
// is dropped, since the caller was promised it would not unwind. The fallback
// line goes last: stdlib log may be redirected into the logger that panicked.
func reportLoggerPanic(ctx context.Context, method string, recovered any) (panicked bool) {
	if recovered == nil {
		return false
	}

	panicked = true

	defer func() { _ = recover() }()

	if ctx == nil {
		ctx = context.Background()
	}

	panicType := fmt.Sprintf("%T", recovered)
	stack := debug.Stack()
	fields := []any{String("method", method), String("panic_type", panicType)}

	// Same posture as runtime's recovered-panic line: the stack only outside
	// production mode. The span always gets it, as runtime's does.
	if !panicobs.ProductionMode() {
		fields = append(fields, String("stack_trace", string(stack)))
	}

	panicobs.Count(ctx, panicComponent, method)
	panicobs.RecordToSpan(ctx, panicType, stack, panicComponent, method)
	panicFallback.Log(ctx, LevelError, loggerPanicMsg, fields...)

	return panicked
}

// universalShim supplies the Logger methods a Universal logger lacks,
// carrying the bound fields and group path it has no way to store itself.
type universalShim struct {
	next   Universal
	fields []Field
	groups []string
}

// Log replays the bound group path and fields ahead of the caller's own, then
// forwards to the wrapped Universal logger.
func (s *universalShim) Log(ctx context.Context, level int, msg string, fields ...any) {
	if s == nil {
		return
	}

	merged := make([]any, 0, len(s.fields)+len(fields)+1)

	if len(s.groups) > 0 {
		merged = append(merged, String(groupKey, strings.Join(s.groups, ".")))
	}

	merged = append(merged, anyFields(s.fields)...)
	merged = append(merged, fields...)

	defer func() { reportLoggerPanic(ctx, "Log", recover()) }()

	s.next.Log(ctx, level, msg, merged...)
}

// With returns a copy carrying the additional fields.
//
//nolint:ireturn
func (s *universalShim) With(fields ...any) Logger {
	if s == nil {
		return NewNop()
	}

	typed := Fields(fields...)

	merged := make([]Field, 0, len(s.fields)+len(typed))
	merged = append(merged, s.fields...)
	merged = append(merged, typed...)

	return &universalShim{next: s.next, fields: merged, groups: cloneStrings(s.groups)}
}

// WithGroup returns a copy scoped under an additional group path segment.
// Empty or whitespace-only names are ignored, matching GoLogger and the zap
// adapter.
//
//nolint:ireturn
func (s *universalShim) WithGroup(name string) Logger {
	if s == nil {
		return NewNop()
	}

	if trimmed := strings.TrimSpace(name); trimmed == "" {
		return s
	}

	groups := make([]string, 0, len(s.groups)+1)
	groups = append(groups, s.groups...)
	groups = append(groups, sanitizeLogString(name))

	fields := make([]Field, 0, len(s.fields))
	fields = append(fields, s.fields...)

	return &universalShim{next: s.next, fields: fields, groups: groups}
}

// Enabled reports the wrapped logger's own answer when it has one, and true
// for any defined level otherwise.
//
// Universal does not require a level check, but many Log-only loggers carry
// one (Enabled(level int) bool) without implementing the rest of Logger.
// Asking it lets callers skip building entries the logger would discard.
// Without one, true is the only answer that cannot silently drop an entry the
// underlying logger would have emitted. An undefined level always reports
// false, matching GoLogger.
func (s *universalShim) Enabled(level int) (enabled bool) {
	if s == nil || !LevelValid(level) {
		return false
	}

	if checker, ok := s.next.(interface{ Enabled(level int) bool }); ok {
		defer func() {
			if reportLoggerPanic(context.Background(), "Enabled", recover()) {
				enabled = true
			}
		}()

		return checker.Enabled(level)
	}

	return true
}

// Sync is a no-op: a Universal logger exposes nothing to flush.
func (s *universalShim) Sync(_ context.Context) error { return nil }

// cloneStrings copies a group path so a bound logger never aliases caller memory.
func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}

	out := make([]string, len(in))
	copy(out, in)

	return out
}

// loggerPanicMsg is the message of the ERROR line reporting a panic recovered
// inside an adapted logger.
const loggerPanicMsg = "logger panic recovered"

// panicComponent labels a panic recovered inside an adapted logger on the
// recovered-panic counter and span event; the method is the name.
const panicComponent = "log"

// panicFallback receives the report of a panic recovered inside an adapted
// logger. It is never the adapted logger: that one just panicked.
var panicFallback Universal = &GoLogger{Level: LevelError}

// errLoggerPanicked is what a guarded logger's Sync returns when it panicked.
var errLoggerPanicked = errors.New("logger panicked")

// Guard adapts any logger, as Adapt does, and wraps the result so that a panic
// inside it never unwinds the caller.
//
// Adapt guards the Log-only loggers it shims but returns a full Logger as-is,
// so callers keep their own instance back. Guard is the opt-in net for the
// rest: call it where a logger you did not write runs on a path that must not
// panic, such as a library boundary that promises to return errors.
//
// A panic inside Log, With, WithGroup, Enabled or Sync is recovered and
// reported exactly as the Adapt shim reports one (see Adapt): the log line is
// dropped, With and WithGroup return the receiver, Enabled answers
// LevelValid(level), and Sync returns an error. Children from With and
// WithGroup are guarded too.
//
// Guard(nil) and a typed nil return NewNop, as Adapt does. This package's own loggers
// (GoLogger, NopLogger, the Adapt shim) and an already guarded logger are
// returned unchanged. Anything else costs one allocation when wrapped and
// about 12 ns per Log call for the deferred recover, with no allocation.
//
//nolint:ireturn // returning the interface is the whole point of the wrapper.
func Guard(u Universal) Logger {
	l := Adapt(u)

	switch l.(type) {
	case *NopLogger, *GoLogger, *universalShim, *panicGuard:
		return l
	default:
		return &panicGuard{next: l}
	}
}

// panicGuard delegates every method to next, recovering its panics.
type panicGuard struct {
	next Logger
}

func (g *panicGuard) Log(ctx context.Context, level int, msg string, fields ...any) {
	defer func() { reportLoggerPanic(ctx, "Log", recover()) }()

	g.next.Log(ctx, level, msg, fields...)
}

//nolint:ireturn
func (g *panicGuard) With(fields ...any) (child Logger) {
	defer func() {
		if reportLoggerPanic(context.Background(), "With", recover()) {
			child = g
		}
	}()

	return Guard(g.next.With(fields...))
}

//nolint:ireturn
func (g *panicGuard) WithGroup(name string) (child Logger) {
	defer func() {
		if reportLoggerPanic(context.Background(), "WithGroup", recover()) {
			child = g
		}
	}()

	return Guard(g.next.WithGroup(name))
}

func (g *panicGuard) Enabled(level int) (enabled bool) {
	defer func() {
		if reportLoggerPanic(context.Background(), "Enabled", recover()) {
			enabled = LevelValid(level)
		}
	}()

	return g.next.Enabled(level)
}

func (g *panicGuard) Sync(ctx context.Context) (err error) {
	defer func() {
		if reportLoggerPanic(ctx, "Sync", recover()) {
			err = errLoggerPanicked
		}
	}()

	return g.next.Sync(ctx)
}
