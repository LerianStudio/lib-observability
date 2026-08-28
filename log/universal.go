package log

import (
	"context"
	"strings"
)

// UniversalLogger mirrors Logger using only universal Go types, so that a
// consumer module can declare an equivalent interface locally, in its own
// package, without importing this one.
//
// # Why this exists
//
// Logger declares its methods in terms of types DEFINED by this package —
// Level (a named uint8) and Field (a named struct). Go interface satisfaction
// is structural, but the types inside a method signature are matched
// NOMINALLY: a type named log.Level and a byte-for-byte identical type named
// mypkg.Level are different types, so a method set built from one never
// satisfies an interface built from the other. A consumer that wants to
// depend on "something that logs" therefore has no choice but to import this
// module for Level and Field — and, importing it, inherits its major version.
// Every major release of lib-observability then propagates through the whole
// fleet in lockstep, even for consumers that only ever call Log.
//
// With/WithGroup make it strictly worse: they are self-returning
// (With(...) Logger), and a self-returning method is unsatisfiable from
// outside the declaring package by construction — no local interface can
// name the return type.
//
// UniversalLogger removes both obstacles. Its signatures use only int,
// string, any, error and context.Context, all universal (predeclared or
// stdlib) types, and it has no self-returning method. A consumer can write
// this in its own package and accept the adapter with no import of
// lib-observability at all:
//
//	type Logger interface {
//		Log(ctx context.Context, level int, msg string, kv ...any)
//		Enabled(level int) bool
//		Sync(ctx context.Context) error
//	}
//
// The composition helpers that would have been self-returning methods are
// free functions instead: UniversalWith and UniversalWithGroup.
//
// # Level scale
//
// level is an int on the SAME numeric scale as Level: Error=0, Warn=1,
// Info=2, Debug=3 (lower is more severe). See LevelErrorInt and friends.
// Values outside that range are handled explicitly, never cast blindly —
// see Log and Enabled.
//
// # Key/value pairs
//
// kv carries alternating key/value pairs in the style of log/slog: a string
// key followed by an arbitrary value. Malformed input is surfaced rather
// than dropped, using slog's BadKey convention — see UniversalBadKey.
type UniversalLogger interface {
	Log(ctx context.Context, level int, msg string, kv ...any)
	Enabled(level int) bool
	Sync(ctx context.Context) error
}

// Universal level constants. They carry the same numeric values as the Level
// constants of the same name, so consumers can hardcode 0..3 (or copy these
// values into their own package) and stay on this package's scale without
// importing it.
//
//	LevelErrorInt (0) -- most severe
//	LevelWarnInt  (1)
//	LevelInfoInt  (2)
//	LevelDebugInt (3) -- least severe
const (
	LevelErrorInt = int(LevelError)
	LevelWarnInt  = int(LevelWarn)
	LevelInfoInt  = int(LevelInfo)
	LevelDebugInt = int(LevelDebug)
)

// LevelUnknownInt is the universal mirror of LevelUnknown. It is not a
// loggable severity: passing it (or any other out-of-range int) follows the
// out-of-range policy documented on universalAdapter.Log and
// universalAdapter.Enabled.
const LevelUnknownInt = int(LevelUnknown)

const (
	// UniversalBadKey is the key used for a kv entry this package cannot
	// interpret: a non-string key, or a trailing key with no value. It is
	// the same sentinel log/slog uses, so operators reading a mixed fleet
	// see one convention.
	UniversalBadKey = "!BADKEY"

	// UniversalBadLevelKey is the key under which the original int is
	// recorded when Log receives a level outside the defined range.
	UniversalBadLevelKey = "!BADLEVEL"

	// universalGroupKey is the key used to carry group names into a foreign
	// UniversalLogger implementation, which has no WithGroup of its own.
	universalGroupKey = "group"
)

// universalAdapter is the UniversalLogger view of a Logger. It is the only
// implementation that can delegate composition to a real Logger, so
// UniversalWith and UniversalWithGroup type-assert for it.
type universalAdapter struct {
	inner Logger
}

// universalBound wraps a UniversalLogger that is NOT one of ours (a consumer
// implementation, a test double) and carries the bound key/value pairs and
// group names it has no way to store itself, replaying them on every Log.
type universalBound struct {
	next   UniversalLogger
	kv     []any
	groups []string
}

// Universal adapts a Logger to the universal form described on
// UniversalLogger.
//
// Universal(nil), and a Logger interface holding a typed-nil implementation,
// both return a working adapter over NewNop rather than a nil interface —
// the result is always safe to call. See IsNil.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func Universal(l Logger) UniversalLogger {
	if IsNil(l) {
		return &universalAdapter{inner: NewNop()}
	}

	return &universalAdapter{inner: l}
}

// UniversalWith returns a UniversalLogger with kv bound to it, the free-function
// equivalent of Logger.With. It is a function rather than a method because a
// self-returning method cannot be declared by a consumer in its own package,
// which would defeat the purpose of UniversalLogger.
//
// Behavior by argument:
//
//   - an adapter returned by Universal — delegates to the underlying
//     Logger.With, so the backing implementation applies its own field
//     handling (redaction, grouping, encoding).
//   - any other UniversalLogger — degrades to a wrapper that prepends kv to
//     every subsequent Log call. Semantically equivalent, but the pairs are
//     re-sent per call instead of being bound once by the implementation.
//   - nil (or typed-nil) — returns the same safe no-op adapter as
//     Universal(nil).
//
// Malformed kv follows the UniversalBadKey convention. An empty kv returns u
// unchanged.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func UniversalWith(u UniversalLogger, kv ...any) UniversalLogger {
	if IsNil(u) {
		return Universal(nil)
	}

	if len(kv) == 0 {
		return u
	}

	switch typed := u.(type) {
	case *universalAdapter:
		return Universal(typed.inner.With(fieldsFromKV(kv)...))
	case *universalBound:
		return typed.with(kv)
	default:
		return &universalBound{next: u, kv: cloneAny(kv)}
	}
}

// UniversalWithGroup returns a UniversalLogger scoped under name, the
// free-function equivalent of Logger.WithGroup. It is a function for the same
// reason as UniversalWith.
//
// Behavior by argument:
//
//   - an adapter returned by Universal — delegates to the underlying
//     Logger.WithGroup.
//   - any other UniversalLogger — degrades to a wrapper that adds a
//     "group" key/value pair, holding the dot-joined group path, to every
//     subsequent Log call. A foreign implementation has no WithGroup to
//     delegate to, so the group is expressed as data instead of structure.
//   - nil (or typed-nil) — returns the same safe no-op adapter as
//     Universal(nil).
//
// An empty or whitespace-only name returns u unchanged, matching GoLogger
// and the zap adapter.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func UniversalWithGroup(u UniversalLogger, name string) UniversalLogger {
	if IsNil(u) {
		return Universal(nil)
	}

	if strings.TrimSpace(name) == "" {
		return u
	}

	switch typed := u.(type) {
	case *universalAdapter:
		return Universal(typed.inner.WithGroup(name))
	case *universalBound:
		return typed.withGroup(name)
	default:
		return &universalBound{next: u, groups: []string{name}}
	}
}

// Log forwards the entry to the wrapped Logger.
//
// Out-of-range policy: a level outside LevelErrorInt..LevelDebugInt is never
// cast blindly onto Level. The entry is emitted at LevelError — the most
// severe level, so a miscalibrated caller is never silently dropped — with
// the original int attached under UniversalBadLevelKey.
func (u *universalAdapter) Log(ctx context.Context, level int, msg string, kv ...any) {
	if u == nil {
		return
	}

	fields := fieldsFromKV(kv)

	resolved, ok := levelFromInt(level)
	if !ok {
		resolved = LevelError

		fields = append(fields, Int(UniversalBadLevelKey, level))
	}

	u.inner.Log(ctx, resolved, msg, fields...)
}

// Enabled reports whether the wrapped Logger emits entries at level.
//
// Out-of-range policy: a level outside LevelErrorInt..LevelDebugInt is not a
// severity this package can answer for, so Enabled reports false without
// consulting the wrapped Logger. Note the asymmetry with Log, which still
// emits such an entry at LevelError: Enabled answers "is this a level you
// should format for?", and the answer for an undefined level is no.
func (u *universalAdapter) Enabled(level int) bool {
	if u == nil {
		return false
	}

	resolved, ok := levelFromInt(level)
	if !ok {
		return false
	}

	return u.inner.Enabled(resolved)
}

// Sync flushes the wrapped Logger.
func (u *universalAdapter) Sync(ctx context.Context) error {
	if u == nil {
		return nil
	}

	return u.inner.Sync(ctx)
}

// Log replays the bound group path and key/value pairs ahead of the caller's
// own, then forwards to the wrapped UniversalLogger.
func (u *universalBound) Log(ctx context.Context, level int, msg string, kv ...any) {
	if u == nil {
		return
	}

	merged := make([]any, 0, len(u.kv)+len(kv)+2)

	if len(u.groups) > 0 {
		merged = append(merged, universalGroupKey, strings.Join(u.groups, "."))
	}

	merged = append(merged, u.kv...)
	merged = append(merged, kv...)

	u.next.Log(ctx, level, msg, merged...)
}

// Enabled delegates to the wrapped UniversalLogger.
func (u *universalBound) Enabled(level int) bool {
	if u == nil {
		return false
	}

	return u.next.Enabled(level)
}

// Sync delegates to the wrapped UniversalLogger.
func (u *universalBound) Sync(ctx context.Context) error {
	if u == nil {
		return nil
	}

	return u.next.Sync(ctx)
}

// with returns a copy carrying the additional key/value pairs.
func (u *universalBound) with(kv []any) *universalBound {
	merged := make([]any, 0, len(u.kv)+len(kv))
	merged = append(merged, u.kv...)
	merged = append(merged, kv...)

	return &universalBound{next: u.next, kv: merged, groups: cloneStrings(u.groups)}
}

// withGroup returns a copy carrying an additional group path segment.
func (u *universalBound) withGroup(name string) *universalBound {
	groups := make([]string, 0, len(u.groups)+1)
	groups = append(groups, u.groups...)
	groups = append(groups, name)

	return &universalBound{next: u.next, kv: cloneAny(u.kv), groups: groups}
}

// levelFromInt maps a universal level int onto Level.
//
// It matches the defined constants exactly and reports false for everything
// else, including LevelUnknownInt: the conversion is never a blind cast, so
// no out-of-range int can reach Level.
func levelFromInt(level int) (Level, bool) {
	switch level {
	case LevelErrorInt, LevelWarnInt, LevelInfoInt, LevelDebugInt:
		// Bounded to 0..3 by the case list above, so the narrowing
		// conversion cannot overflow uint8.
		return Level(level), true
	default:
		return LevelUnknown, false
	}
}

// fieldsFromKV converts slog-style alternating key/value pairs into Fields.
//
// It follows log/slog's argsToAttr rules exactly, so a malformed call is
// visible in the output instead of being silently dropped:
//
//   - a non-string key becomes Field{UniversalBadKey, thatValue} and consumes
//     one element (the offending element is treated as a value);
//   - a trailing string key with no value becomes
//     Field{UniversalBadKey, thatKey} and consumes one element;
//   - a well-formed pair becomes Field{key, value} and consumes two.
//
// An empty or nil kv yields no fields.
func fieldsFromKV(kv []any) []Field {
	if len(kv) == 0 {
		return nil
	}

	fields := make([]Field, 0, (len(kv)+1)/2)

	for i := 0; i < len(kv); {
		key, ok := kv[i].(string)
		if !ok {
			fields = append(fields, Any(UniversalBadKey, kv[i]))
			i++

			continue
		}

		if i+1 >= len(kv) {
			fields = append(fields, Any(UniversalBadKey, key))
			i++

			continue
		}

		fields = append(fields, Any(key, kv[i+1]))
		i += 2
	}

	return fields
}

// cloneAny copies a kv slice so a bound logger never aliases caller memory.
func cloneAny(kv []any) []any {
	if len(kv) == 0 {
		return nil
	}

	out := make([]any, len(kv))
	copy(out, kv)

	return out
}

// cloneStrings copies a group path so a bound logger never aliases caller memory.
func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}

	out := make([]string, len(in))
	copy(out, in)

	return out
}
