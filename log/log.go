package log

import (
	"context"
	"fmt"
	"strings"
)

// Logger is the package logging interface.
//
// # No defined type of this package appears in any method signature
//
// Every parameter and every variadic element below is a universal type:
// context.Context, int, string, any, error. That is deliberate, and it is the
// central design constraint of this package.
//
// Go matches the types inside a method signature NOMINALLY. A method
// Log(context.Context, v3/log.Level, string, ...v3/log.Field) does not satisfy
// an interface declaring Log(context.Context, v4/log.Level, string,
// ...v4/log.Field), even when Level and Field are byte-for-byte identical in
// both versions — the two Levels are simply different types. A consumer that
// wanted to depend on "something that logs" therefore had no choice but to
// import this module for Level and Field, and importing it, inherited its
// major version. Every major release of lib-observability then propagated
// through the whole fleet in lockstep, including to consumers that never
// touched the thing the major was actually about.
//
// With universal types in the signature, a consumer can declare this in its
// own package, import nothing, and accept any logger this module produces from
// v4 onward - forever, across every future major:
//
//	type Logger interface {
//		Log(ctx context.Context, level int, msg string, fields ...any)
//	}
//
// Level and Field still exist — see below — but only as values, never as
// part of a signature a foreign package would have to name.
//
// # Level scale
//
// level is on this package's scale: Error=0, Warn=1, Info=2, Debug=3. Lower
// is more severe. Note this is INVERTED from log/slog (Debug=-4..Error=8);
// the constants below are the source of truth. An out-of-range level is
// never dropped silently — see GoLogger.Log.
//
// # Fields
//
// fields accepts, in any mix:
//
//   - a Field produced by String, Int, Bool, Err or Any - the typed form,
//     and still the preferred one;
//   - a *Field, which is dereferenced (a nil one is skipped);
//   - a []Field, which carries several entries as ONE variadic element and is
//     flattened in place. This is how a caller holding a slice passes it, since
//     a []Field can no longer be spread into a ...any variadic;
//   - slog-style alternating pairs: a string key followed by any value.
//
// Malformed input is surfaced under BadKey rather than dropped. See Fields.
//
//go:generate mockgen --destination=log_mock.go --package=log . Logger
type Logger interface {
	Log(ctx context.Context, level int, msg string, fields ...any)
	With(fields ...any) Logger
	WithGroup(name string) Logger
	Enabled(level int) bool
	Sync(ctx context.Context) error
}

// Level is the defined-type form of a severity, kept for configuration and
// parsing: it carries String and is what ParseLevel returns, so a level read
// from an env var stays self-describing.
//
// Level deliberately does NOT appear in the Logger interface. Pass severities
// to Logger.Log as the untyped constants below (or a plain int); convert an
// explicitly-typed Level with int(lvl) at the call.
//
// Lower numeric values indicate higher severity (LevelError=0 is most severe,
// LevelDebug=3 is least). This is inverted from slog/zap conventions where
// higher numeric values mean higher severity.
//
// The GoLogger.Enabled comparison uses l.Level >= level, which works because
// the logger's Level acts as a verbosity ceiling: a logger at LevelInfo (2)
// emits Error (0), Warn (1), and Info (2) messages, but suppresses Debug (3).
type Level uint8

const (
	levelDebugString = "debug"
	levelInfoString  = "info"
	levelWarnString  = "warn"
	levelErrorString = "error"

	errorFieldKey = "error"
)

// Level constants define log severity. Lower numeric values indicate higher
// severity. Setting a logger's Level to a given constant enables that level
// and all levels with lower numeric values (i.e., higher severity).
//
//	LevelError (0) -- only errors
//	LevelWarn  (1) -- errors + warnings
//	LevelInfo  (2) -- errors + warnings + info
//	LevelDebug (3) -- everything
//
// These are UNTYPED constants on purpose. An untyped constant converts
// implicitly to both int and Level, so one spelling works everywhere:
//
//	logger.Log(ctx, log.LevelError, "boom", log.Err(err)) // int parameter
//	cfg := &log.GoLogger{Level: log.LevelInfo}            // Level field
//
// Typing them as Level instead would break every Logger.Log call in the
// fleet, which is exactly the churn this package is trying to stop causing.
//
// This MUST be its own const block: iota counts every ConstSpec in the block
// it appears in, not just the ones that use it. Sharing a block with the
// four string constants above previously put LevelError at iota=5 (not 0),
// silently contradicting the doc comment and breaking every ordering
// comparison against it - GoLogger.Enabled's `l.Level >= level` in
// particular, which made the Level-0 zero value (an uninitialized
// &GoLogger{}, e.g. WithHTTPLogging's default with no WithCustomLogger)
// enabled for NOTHING, since 0 >= 5 is false for every defined level.
const (
	LevelError = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

// LevelUnknown represents an invalid or unrecognized log level.
// Returned by ParseLevel on error to distinguish from LevelError (the zero
// value). Untyped for the same reason as the constants above.
const LevelUnknown = 255

const (
	// BadKey is the key used for a fields entry this package cannot
	// interpret: a non-string key, or a trailing key with no value. It is the
	// same sentinel log/slog uses, so operators reading a mixed fleet see one
	// convention.
	BadKey = "!BADKEY"

	// BadLevelKey is the key under which the original int is recorded when
	// Log receives a level outside LevelError..LevelDebug.
	BadLevelKey = "!BADLEVEL"

	// groupKey is the key used to carry a group path into a Universal logger,
	// which has no WithGroup of its own. See Adapt.
	groupKey = "group"
)

// String returns the string representation of a log level.
func (level Level) String() string {
	return LevelName(int(level))
}

// LevelName returns the string representation of a severity given as a plain
// int, the form the Logger interface uses. Out-of-range values yield
// "unknown".
func LevelName(level int) string {
	switch level {
	case LevelDebug:
		return levelDebugString
	case LevelInfo:
		return levelInfoString
	case LevelWarn:
		return levelWarnString
	case LevelError:
		return levelErrorString
	default:
		return "unknown"
	}
}

// LevelFrom maps a severity given as a plain int - the form the Logger
// interface uses - onto the defined Level type, reporting whether it was one
// of the four defined severities.
//
// It matches the constants exactly and never performs a narrowing conversion,
// so no out-of-range int can reach Level. Use it wherever a universal level
// has to cross back into the typed world.
func LevelFrom(level int) (Level, bool) {
	switch level {
	case LevelError:
		return LevelError, true
	case LevelWarn:
		return LevelWarn, true
	case LevelInfo:
		return LevelInfo, true
	case LevelDebug:
		return LevelDebug, true
	default:
		return LevelUnknown, false
	}
}

// LevelValid reports whether level is one of the four defined severities.
// Use it to distinguish a miscalibrated caller from a legitimate entry
// without converting an arbitrary int onto Level.
func LevelValid(level int) bool {
	switch level {
	case LevelError, LevelWarn, LevelInfo, LevelDebug:
		return true
	default:
		return false
	}
}

// ParseLevel takes a string level and returns a Level constant.
// Leading and trailing whitespace is trimmed before matching.
func ParseLevel(lvl string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(lvl)) {
	case levelDebugString:
		return LevelDebug, nil
	case levelInfoString:
		return LevelInfo, nil
	case levelWarnString, "warning":
		return LevelWarn, nil
	case levelErrorString:
		return LevelError, nil
	}

	return LevelUnknown, fmt.Errorf("not a valid Level: %q", lvl)
}

// Field is a strongly-typed key/value attribute attached to a log event.
//
// Field is still the preferred way to attach structured data. It no longer
// appears in the Logger interface — the variadic there is ...any — but a
// Field passed through that variadic is recognized and rendered exactly as
// before, so existing call sites are unaffected.
type Field struct {
	Key   string
	Value any
}

// Any creates a field with an arbitrary value.
//
// WARNING: prefer typed constructors (String, Int, Bool, Err) to avoid
// accidentally logging sensitive data (passwords, tokens, PII). If using
// Any, ensure the value is sanitized or non-sensitive.
func Any(key string, value any) Field {
	return Field{Key: key, Value: value}
}

// String creates a string field.
func String(key, value string) Field {
	return Field{Key: key, Value: value}
}

// Int creates an integer field.
func Int(key string, value int) Field {
	return Field{Key: key, Value: value}
}

// Bool creates a boolean field.
func Bool(key string, value bool) Field {
	return Field{Key: key, Value: value}
}

// Err creates the conventional `error` field.
func Err(err error) Field {
	return Field{Key: errorFieldKey, Value: err}
}

// Fields normalizes the ...any variadic of the Logger interface into typed
// Fields. It is the single conversion point between the universal boundary
// and this package's internals, and implementations of Logger outside this
// package are welcome to use it.
//
// Rules, scanning left to right:
//
//   - a Field (or *Field) is taken as-is and consumes one element;
//   - a []Field is flattened in place and consumes one element, so a caller
//     holding a slice can pass it as a single argument;
//   - a string key followed by at least one more element becomes
//     Field{key, next} and consumes two;
//   - a trailing string key with no value becomes Field{BadKey, key} and
//     consumes one;
//   - anything else in key position becomes Field{BadKey, value} and
//     consumes one.
//
// The last three rules follow log/slog's argsToAttr exactly, so a malformed
// call is visible in the output instead of being silently dropped.
//
// A nil or empty args yields nil.
func Fields(args ...any) []Field {
	if len(args) == 0 {
		return nil
	}

	out := make([]Field, 0, len(args))

	for i := 0; i < len(args); {
		switch value := args[i].(type) {
		case Field:
			out = append(out, value)
			i++
		case *Field:
			if value != nil {
				out = append(out, *value)
			}

			i++
		case []Field:
			out = append(out, value...)
			i++
		case string:
			if i+1 >= len(args) {
				out = append(out, Any(BadKey, value))
				i++

				continue
			}

			out = append(out, Any(value, args[i+1]))
			i += 2
		default:
			out = append(out, Any(BadKey, args[i]))
			i++
		}
	}

	return out
}

// anyFields widens typed Fields back into the ...any form the Logger
// interface takes, for implementations that hold []Field internally and need
// to forward to another Logger.
func anyFields(fields []Field) []any {
	if len(fields) == 0 {
		return nil
	}

	out := make([]any, len(fields))
	for i, f := range fields {
		out[i] = f
	}

	return out
}
