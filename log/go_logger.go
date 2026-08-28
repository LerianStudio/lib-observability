package log

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/LerianStudio/lib-observability/v4/redaction"
)

var logControlCharReplacer = strings.NewReplacer(
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
	"\x00", `\0`,
)

func sanitizeLogString(s string) string {
	return logControlCharReplacer.Replace(s)
}

// GoLogger is the stdlib logger implementation for Logger.
type GoLogger struct {
	Level  Level
	fields []Field
	groups []string
}

// Enabled reports whether the logger emits entries at the given level.
// On a nil receiver, Enabled returns false silently. Use NopLogger as the
// documented nil-safe alternative.
//
// Unknown level policy: a level outside the defined range
// (LevelError..LevelDebug) is not a severity this package can answer for, so
// Enabled reports false without consulting the configured threshold. Note the
// asymmetry with Log, which still EMITS such an entry (at LevelError, tagged
// with BadLevelKey): Enabled answers "is this a level you should format for?",
// and the answer for an undefined level is no.
func (l *GoLogger) Enabled(level int) bool {
	if l == nil {
		return false
	}

	if !LevelValid(level) {
		return false
	}

	return int(l.Level) >= level
}

// Log writes a single log line if the level is enabled.
//
// fields follows the Logger contract: Fields, []Field and slog-style
// alternating key/value pairs are all accepted. See Fields.
//
// Out-of-range policy: a level outside LevelError..LevelDebug is never cast
// blindly onto Level. The entry is emitted at LevelError - the most severe
// level, so a miscalibrated caller is never silently dropped - with the
// original int attached under BadLevelKey.
func (l *GoLogger) Log(_ context.Context, level int, msg string, fields ...any) {
	typed := Fields(fields...)

	if !LevelValid(level) {
		typed = append(typed, Int(BadLevelKey, level))
		level = LevelError
	}

	if !l.Enabled(level) {
		return
	}

	line := l.hydrateLine(level, msg, typed...)
	log.Print(line)
}

// With returns a child logger with additional persistent fields.
//
//nolint:ireturn
func (l *GoLogger) With(fields ...any) Logger {
	if l == nil {
		return &NopLogger{}
	}

	typed := Fields(fields...)

	newFields := make([]Field, 0, len(l.fields)+len(typed))
	newFields = append(newFields, l.fields...)
	newFields = append(newFields, typed...)

	newGroups := make([]string, 0, len(l.groups))
	newGroups = append(newGroups, l.groups...)

	return &GoLogger{
		Level:  l.Level,
		fields: newFields,
		groups: newGroups,
	}
}

// WithGroup returns a child logger scoped under the provided group name.
// Empty or whitespace-only names are silently ignored, consistent with
// the zap adapter. This avoids creating unnecessary allocations.
//
//nolint:ireturn
func (l *GoLogger) WithGroup(name string) Logger {
	if l == nil {
		return &NopLogger{}
	}

	if strings.TrimSpace(name) == "" {
		return l
	}

	newGroups := make([]string, 0, len(l.groups)+1)
	newGroups = append(newGroups, l.groups...)
	newGroups = append(newGroups, sanitizeLogString(name))

	newFields := make([]Field, 0, len(l.fields))
	newFields = append(newFields, l.fields...)

	return &GoLogger{
		Level:  l.Level,
		fields: newFields,
		groups: newGroups,
	}
}

// Sync flushes buffered logs. It is a no-op for the stdlib logger.
func (l *GoLogger) Sync(_ context.Context) error { return nil }

func (l *GoLogger) hydrateLine(level int, msg string, fields ...Field) string {
	parts := make([]string, 0, 4)
	parts = append(parts, fmt.Sprintf("[%s]", LevelName(level)))

	if l != nil && len(l.groups) > 0 {
		parts = append(parts, fmt.Sprintf("[group=%s]", strings.Join(l.groups, ".")))
	}

	allFields := make([]Field, 0, len(fields))
	if l != nil {
		allFields = append(allFields, l.fields...)
	}

	allFields = append(allFields, fields...)

	if rendered := renderFields(allFields); rendered != "" {
		parts = append(parts, rendered)
	}

	parts = append(parts, sanitizeLogString(msg))

	return strings.Join(parts, " ")
}

// redactedValue is the placeholder used for sensitive field values in log output.
const redactedValue = "[REDACTED]"

func renderFields(fields []Field) string {
	if len(fields) == 0 {
		return ""
	}

	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		key := sanitizeLogString(field.Key)
		if key == "" {
			continue
		}

		var rendered any
		if redaction.IsSensitiveField(field.Key) {
			rendered = redactedValue
		} else {
			rendered = sanitizeFieldValue(field.Value)
		}

		parts = append(parts, fmt.Sprintf("%s=%v", key, rendered))
	}

	if len(parts) == 0 {
		return ""
	}

	return fmt.Sprintf("[%s]", strings.Join(parts, ", "))
}

// isTypedNil reports whether v is a non-nil interface wrapping a nil pointer.
// This prevents panics when calling methods (Error, String) on typed-nil values.
func isTypedNil(v any) bool {
	if v == nil {
		return false
	}

	rv := reflect.ValueOf(v)

	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

func sanitizeFieldValue(value any) any {
	if value == nil {
		return nil
	}

	// Guard against typed-nil before calling interface methods.
	if isTypedNil(value) {
		return "<nil>"
	}

	switch v := value.(type) {
	case string:
		return sanitizeLogString(v)
	case error:
		// SafeErrorMessage, not v.Error() directly: a valid, non-nil error
		// (e.g. errors.Join with a typed-nil member) can still panic when
		// stringified even past the isTypedNil guard above, since that guard
		// only inspects the top-level value.
		return sanitizeLogString(SafeErrorMessage(v))
	case fmt.Stringer:
		return sanitizeLogString(v.String())
	case bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		// Primitive types cannot carry newlines; pass through unchanged.
		return value
	default:
		// Composite types (structs, slices, maps, etc.) may carry raw newlines
		// when rendered with fmt. Pre-serialize and sanitize the result.
		return sanitizeLogString(fmt.Sprintf("%v", v))
	}
}
