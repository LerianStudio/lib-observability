package log

import (
	"context"
	"strings"
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
// reports true for any defined level - a Log-only logger exposes no level
// check, and answering false would silently drop entries. Sync is a no-op.
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

// Enabled reports true for any defined level.
//
// A Universal logger exposes no level check, so this is the only answer that
// cannot silently drop an entry the underlying logger would have emitted. An
// undefined level still reports false, matching GoLogger.
func (s *universalShim) Enabled(level int) bool {
	if s == nil {
		return false
	}

	return LevelValid(level)
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
