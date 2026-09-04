//go:build unit

package log

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGoLogger_OutOfRangeLevelIsEmittedAtError pins the out-of-range policy.
//
// A level outside LevelError..LevelDebug is never cast blindly onto Level and
// never silently dropped: the entry is emitted at the MOST severe level so a
// miscalibrated caller is loud rather than invisible, and the original int is
// attached under BadLevelKey so the miscalibration is diagnosable.
func TestGoLogger_OutOfRangeLevelIsEmittedAtError(t *testing.T) {
	tests := []struct {
		name  string
		level int
	}{
		{name: "just above debug", level: 4},
		{name: "negative", level: -1},
		{name: "unknown sentinel", level: LevelUnknown},
		{name: "far out of range", level: 99999},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			withTestLoggerOutput(t, &buf)

			// Level: LevelError is the STRICTEST threshold. The entry must
			// still appear, proving the demotion to LevelError happens before
			// the Enabled check rather than after it.
			logger := &GoLogger{Level: LevelError}
			logger.Log(context.Background(), tt.level, "miscalibrated", String("k", "v"))

			out := buf.String()
			assert.Contains(t, out, "[error]")
			assert.NotContains(t, out, "[unknown]")
			assert.Contains(t, out, fmt.Sprintf("%s=%d", BadLevelKey, tt.level))
			assert.Contains(t, out, "k=v", "the caller's own fields survive the demotion")
			assert.Contains(t, out, "miscalibrated")
		})
	}
}

// TestGoLogger_OutOfRangeLevelKeepsFieldOrder documents that the BADLEVEL
// field is appended after the caller's fields, not merged into them.
func TestGoLogger_OutOfRangeLevelKeepsFieldOrder(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	(&GoLogger{Level: LevelDebug}).Log(context.Background(), 7, "msg", String("first", "1"))

	out := buf.String()
	require.Contains(t, out, "first=1")
	assert.Less(t, strings.Index(out, "first=1"), strings.Index(out, BadLevelKey))
}

// TestGoLogger_EnabledRejectsOutOfRange records the deliberate asymmetry with
// Log: Enabled answers "is this a level you should format for?", and for an
// undefined level the answer is no - even on the most permissive logger.
func TestGoLogger_EnabledRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	logger := &GoLogger{Level: LevelDebug}

	for _, level := range []int{-1, 4, LevelUnknown, 1 << 20} {
		assert.False(t, logger.Enabled(level), "Enabled(%d)", level)
	}

	for _, level := range []int{LevelError, LevelWarn, LevelInfo, LevelDebug} {
		assert.True(t, logger.Enabled(level), "Enabled(%d)", level)
	}
}

func TestGoLogger_NilReceiverEnabledIsFalse(t *testing.T) {
	t.Parallel()

	var logger *GoLogger

	assert.False(t, logger.Enabled(LevelError))
	assert.IsType(t, &NopLogger{}, logger.With(String("k", "v")))
	assert.IsType(t, &NopLogger{}, logger.WithGroup("g"))
	assert.NoError(t, logger.Sync(context.Background()))
}

// TestGoLogger_AcceptsSlogStylePairs proves the universal ...any variadic
// carries slog-style pairs end to end, not just typed Fields.
func TestGoLogger_AcceptsSlogStylePairs(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	(&GoLogger{Level: LevelDebug}).Log(
		context.Background(), LevelInfo, "mixed",
		"tenant", "t-1",
		Int("status", 200),
		[]Field{Bool("ok", true)},
		"orphan",
	)

	out := buf.String()
	assert.Contains(t, out, "tenant=t-1")
	assert.Contains(t, out, "status=200")
	assert.Contains(t, out, "ok=true")
	assert.Contains(t, out, BadKey+"=orphan")
}

// localUniversalLogger implements ONLY the one-method Universal shape and is
// declared here to stand in for a consumer's own logger type.
type localUniversalLogger struct {
	msgs []string
}

func (l *localUniversalLogger) Log(_ context.Context, level int, msg string, fields ...any) {
	l.msgs = append(l.msgs, fmt.Sprintf("%s|%s|%v", LevelName(level), msg, Fields(fields...)))
}

// TestSafeError_AcceptsLogOnlyLogger: SafeError takes Universal, so a caller
// can hand it a logger that implements nothing else.
func TestSafeError_AcceptsLogOnlyLogger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		production bool
		wantSubstr string
		notSubstr  string
	}{
		{name: "development logs the error", production: false, wantSubstr: "sensitive detail"},
		{
			name:       "production logs only the type",
			production: true,
			wantSubstr: "error_type",
			notSubstr:  "sensitive detail",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sink := &localUniversalLogger{}
			SafeError(sink, context.Background(), "failed", errNoisy, tt.production)

			require.Len(t, sink.msgs, 1)
			assert.Contains(t, sink.msgs[0], "error|failed")
			assert.Contains(t, sink.msgs[0], tt.wantSubstr)

			if tt.notSubstr != "" {
				assert.NotContains(t, sink.msgs[0], tt.notSubstr)
			}
		})
	}
}

type noisyError struct{}

func (noisyError) Error() string { return "sensitive detail" }

var errNoisy = noisyError{}

// TestLevelFrom pins the bounded conversion from the universal int form back
// onto the defined Level type. It must never perform a blind narrowing cast:
// that is what keeps an out-of-range int from reaching a uint8 Level.
func TestLevelFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input int
		want  Level
		ok    bool
	}{
		{"error", LevelError, LevelError, true},
		{"warn", LevelWarn, LevelWarn, true},
		{"info", LevelInfo, LevelInfo, true},
		{"debug", LevelDebug, LevelDebug, true},
		{"above range", 4, LevelUnknown, false},
		{"negative", -1, LevelUnknown, false},
		{"the unknown sentinel is not a severity", LevelUnknown, LevelUnknown, false},
		{"beyond uint8, must not wrap", 256, LevelUnknown, false},
		{"far beyond uint8, must not wrap", 1024, LevelUnknown, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := LevelFrom(tt.input)
			if ok != tt.ok {
				t.Fatalf("LevelFrom(%d) ok = %v, want %v", tt.input, ok, tt.ok)
			}

			if got != tt.want {
				t.Fatalf("LevelFrom(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
