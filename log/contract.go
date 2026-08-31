package log

import (
	"context"
	"strings"
)

// Contract mirrors Logger using only universal Go types, so that a
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
// Contract removes both obstacles. Its signatures use only int,
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
// free functions instead: ContractWith and ContractWithGroup.
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
// than dropped, using slog's BadKey convention — see ContractBadKey.
type Contract interface {
	Log(ctx context.Context, level int, msg string, kv ...any)
	Enabled(level int) bool
	Sync(ctx context.Context) error
}

// Contract level constants. They carry the same numeric values as the Level
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
// out-of-range policy documented on contractAdapter.Log and
// contractAdapter.Enabled.
const LevelUnknownInt = int(LevelUnknown)

const (
	// ContractBadKey is the key used for a kv entry this package cannot
	// interpret: a non-string key, or a trailing key with no value. It is
	// the same sentinel log/slog uses, so operators reading a mixed fleet
	// see one convention.
	ContractBadKey = "!BADKEY"

	// ContractBadLevelKey is the key under which the original int is
	// recorded when Log receives a level outside the defined range.
	ContractBadLevelKey = "!BADLEVEL"

	// universalGroupKey is the key used to carry group names into a foreign
	// Contract implementation, which has no WithGroup of its own.
	universalGroupKey = "group"
)

// contractAdapter is the Contract view of a Logger. It is the only
// implementation that can delegate composition to a real Logger, so
// ContractWith and ContractWithGroup type-assert for it.
type contractAdapter struct {
	inner Logger
}

// contractBound wraps a Contract that is NOT one of ours (a consumer
// implementation, a test double) and carries the bound key/value pairs and
// group names it has no way to store itself, replaying them on every Log.
type contractBound struct {
	next   Contract
	kv     []any
	groups []string
}

// AsContract adapts a Logger to the contract form described on
// Contract.
//
// AsContract(nil), and a Logger interface holding a typed-nil implementation,
// both return a working adapter over NewNop rather than a nil interface —
// the result is always safe to call. See IsNil.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func AsContract(l Logger) Contract {
	if IsNil(l) {
		return &contractAdapter{inner: NewNop()}
	}

	return &contractAdapter{inner: l}
}

// ContractWith returns a Contract with kv bound to it, the free-function
// equivalent of Logger.With. It is a function rather than a method because a
// self-returning method cannot be declared by a consumer in its own package,
// which would defeat the purpose of Contract.
//
// Behavior by argument:
//
//   - an adapter returned by AsContract — delegates to the underlying
//     Logger.With, so the backing implementation applies its own field
//     handling (redaction, grouping, encoding).
//   - any other Contract — degrades to a wrapper that prepends kv to
//     every subsequent Log call. Semantically equivalent, but the pairs are
//     re-sent per call instead of being bound once by the implementation.
//   - nil (or typed-nil) — returns the same safe no-op adapter as
//     AsContract(nil).
//
// Malformed kv follows the ContractBadKey convention. An empty kv returns u
// unchanged.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func ContractWith(c Contract, kv ...any) Contract {
	if IsNil(c) {
		return AsContract(nil)
	}

	if len(kv) == 0 {
		return c
	}

	switch typed := c.(type) {
	case *contractAdapter:
		return AsContract(typed.inner.With(fieldsFromKV(kv)...))
	case *contractBound:
		return typed.with(normalizedKV(kv))
	default:
		return &contractBound{next: c, kv: normalizedKV(kv)}
	}
}

// ContractWithGroup returns a Contract scoped under name, the
// free-function equivalent of Logger.WithGroup. It is a function for the same
// reason as ContractWith.
//
// Behavior by argument:
//
//   - an adapter returned by AsContract — delegates to the underlying
//     Logger.WithGroup.
//   - any other Contract — degrades to a wrapper that adds a
//     "group" key/value pair, holding the dot-joined group path, to every
//     subsequent Log call. A foreign implementation has no WithGroup to
//     delegate to, so the group is expressed as data instead of structure.
//   - nil (or typed-nil) — returns the same safe no-op adapter as
//     AsContract(nil).
//
// An empty or whitespace-only name returns u unchanged, matching GoLogger
// and the zap adapter.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func ContractWithGroup(c Contract, name string) Contract {
	if IsNil(c) {
		return AsContract(nil)
	}

	if strings.TrimSpace(name) == "" {
		return c
	}

	switch typed := c.(type) {
	case *contractAdapter:
		return AsContract(typed.inner.WithGroup(name))
	case *contractBound:
		return typed.withGroup(name)
	default:
		return &contractBound{next: c, groups: []string{name}}
	}
}

// Log forwards the entry to the wrapped Logger.
//
// Out-of-range policy: a level outside LevelErrorInt..LevelDebugInt is never
// cast blindly onto Level. The entry is emitted at LevelError — the most
// severe level, so a miscalibrated caller is never silently dropped — with
// the original int attached under ContractBadLevelKey.
func (u *contractAdapter) Log(ctx context.Context, level int, msg string, kv ...any) {
	if u == nil {
		return
	}

	fields := fieldsFromKV(kv)

	resolved, ok := levelFromInt(level)
	if !ok {
		resolved = LevelError

		fields = append(fields, Int(ContractBadLevelKey, level))
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
func (u *contractAdapter) Enabled(level int) bool {
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
func (u *contractAdapter) Sync(ctx context.Context) error {
	if u == nil {
		return nil
	}

	return u.inner.Sync(ctx)
}

// Log replays the bound group path and key/value pairs ahead of the caller's
// own, then forwards to the wrapped Contract.
func (u *contractBound) Log(ctx context.Context, level int, msg string, kv ...any) {
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

// Enabled delegates to the wrapped Contract.
func (u *contractBound) Enabled(level int) bool {
	if u == nil {
		return false
	}

	return u.next.Enabled(level)
}

// Sync delegates to the wrapped Contract.
func (u *contractBound) Sync(ctx context.Context) error {
	if u == nil {
		return nil
	}

	return u.next.Sync(ctx)
}

// with returns a copy carrying the additional key/value pairs.
func (u *contractBound) with(kv []any) *contractBound {
	merged := make([]any, 0, len(u.kv)+len(kv))
	merged = append(merged, u.kv...)
	merged = append(merged, kv...)

	return &contractBound{next: u.next, kv: merged, groups: cloneStrings(u.groups)}
}

// withGroup returns a copy carrying an additional group path segment.
func (u *contractBound) withGroup(name string) *contractBound {
	groups := make([]string, 0, len(u.groups)+1)
	groups = append(groups, u.groups...)
	groups = append(groups, name)

	return &contractBound{next: u.next, kv: cloneAny(u.kv), groups: groups}
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
//   - a non-string key becomes Field{ContractBadKey, thatValue} and consumes
//     one element (the offending element is treated as a value);
//   - a trailing string key with no value becomes
//     Field{ContractBadKey, thatKey} and consumes one element;
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
			fields = append(fields, Any(ContractBadKey, kv[i]))
			i++

			continue
		}

		if i+1 >= len(kv) {
			fields = append(fields, Any(ContractBadKey, key))
			i++

			continue
		}

		fields = append(fields, Any(key, kv[i+1]))
		i += 2
	}

	return fields
}

// normalizedKV runs kv through the same malformed-input rules the adapter path
// applies, then flattens the result back to alternating pairs.
//
// Without it the two paths disagree: pairs bound onto an adapter are normalized
// on the way in, while pairs bound onto a FOREIGN Contract were stored raw and
// forwarded raw, so a non-string key reached the caller as itself instead of
// under ContractBadKey. Same call, same documented contract, two behaviours.
func normalizedKV(kv []any) []any {
	fields := fieldsFromKV(kv)
	out := make([]any, 0, len(fields)*2)

	for _, field := range fields {
		out = append(out, field.Key, field.Value)
	}

	return out
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
