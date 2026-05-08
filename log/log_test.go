//go:build unit

package log

import (
	"bytes"
	"context"
	"errors"
	stdlog "log"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

var stdLoggerOutputMu sync.Mutex

func withTestLoggerOutput(t *testing.T, output *bytes.Buffer) {
	t.Helper()

	stdLoggerOutputMu.Lock()
	defer t.Cleanup(func() {
		stdLoggerOutputMu.Unlock()
	})

	originalOutput := stdlog.Writer()
	stdlog.SetOutput(output)
	t.Cleanup(func() { stdlog.SetOutput(originalOutput) })
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in       string
		expected Level
		err      bool
	}{
		{in: "error", expected: LevelError},
		{in: "warn", expected: LevelWarn},
		{in: "warning", expected: LevelWarn},
		{in: "info", expected: LevelInfo},
		{in: "debug", expected: LevelDebug},
		{in: "panic", err: true},
		{in: "fatal", err: true},
		{in: "INVALID", err: true},
	}

	for _, tt := range tests {
		level, err := ParseLevel(tt.in)
		if tt.err {
			assert.Error(t, err)
			continue
		}

		assert.NoError(t, err)
		assert.Equal(t, tt.expected, level)
	}
}

func TestGoLogger_Enabled(t *testing.T) {
	logger := &GoLogger{Level: LevelInfo}
	assert.True(t, logger.Enabled(LevelError))
	assert.True(t, logger.Enabled(LevelInfo))
	assert.False(t, logger.Enabled(LevelDebug))
}

func TestGoLogger_LogWithFieldsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := (&GoLogger{Level: LevelDebug}).
		WithGroup("http").
		With(String("request_id", "r-1"))

	logger.Log(context.Background(), LevelInfo, "request finished", Int("status", 200))

	out := buf.String()
	assert.Contains(t, out, "[info]")
	assert.Contains(t, out, "group=http")
	assert.Contains(t, out, "request_id=r-1")
	assert.Contains(t, out, "status=200")
	assert.Contains(t, out, "request finished")
}

func TestGoLogger_WithIsImmutable(t *testing.T) {
	base := &GoLogger{Level: LevelDebug}
	withField := base.With(String("k", "v"))

	assert.NotEqual(t, base, withField)
	assert.Empty(t, base.fields)

	goLogger, ok := withField.(*GoLogger)
	require.True(t, ok, "expected *GoLogger from With()")
	assert.Len(t, goLogger.fields, 1)
}

func TestNopLogger(t *testing.T) {
	nop := NewNop()
	assert.NotPanics(t, func() {
		nop.Log(context.Background(), LevelInfo, "hello")
		_ = nop.With(String("k", "v"))
		_ = nop.WithGroup("x")
		_ = nop.Sync(context.Background())
	})
	assert.False(t, nop.Enabled(LevelError))
}

func TestLevelLegacyNamesRejected(t *testing.T) {
	_, panicErr := ParseLevel("panic")
	_, fatalErr := ParseLevel("fatal")
	assert.Error(t, panicErr)
	assert.Error(t, fatalErr)
}

// TestNoLegacyLevelSymbolsInAPI verifies that ParseLevel rejects legacy level
// names ("panic", "fatal") that were removed in the v2 API migration.
func TestNoLegacyLevelSymbolsInAPI(t *testing.T) {
	legacyNames := []string{"panic", "fatal", "PANIC", "FATAL", "Panic", "Fatal"}
	for _, name := range legacyNames {
		level, err := ParseLevel(name)
		assert.Error(t, err, "ParseLevel(%q) should reject legacy level name", name)
		assert.Equal(t, LevelUnknown, level,
			"ParseLevel(%q) should return LevelUnknown for rejected names", name)
	}

	// Confirm no level constant stringifies to legacy names
	for _, lvl := range []Level{LevelError, LevelWarn, LevelInfo, LevelDebug} {
		s := lvl.String()
		assert.NotEqual(t, "panic", s, "no Level constant should stringify to 'panic'")
		assert.NotEqual(t, "fatal", s, "no Level constant should stringify to 'fatal'")
	}
}

// ===========================================================================
// CWE-117: Log Injection Prevention Tests
// ===========================================================================

func TestCWE117_MessageNewlineInjection(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "LF newline injection",
			input: "legitimate message\n[info] forged log entry",
		},
		{
			name:  "CR injection",
			input: "legitimate message\r[info] forged log entry",
		},
		{
			name:  "CRLF injection",
			input: "legitimate message\r\n[info] forged log entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			withTestLoggerOutput(t, &buf)

			logger := &GoLogger{Level: LevelDebug}
			logger.Log(context.Background(), LevelInfo, tt.input)

			out := buf.String()
			newlineCount := strings.Count(out, "\n")
			assert.Equal(t, 1, newlineCount,
				"CWE-117: log output must be a single line, got %d newlines in: %q", newlineCount, out)

			assert.NotContains(t, out, "\n[info] forged")
		})
	}
}

func TestCWE117_FieldValueInjection(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	maliciousValue := "normal_user\n[error] ADMIN ACCESS GRANTED user=admin action=delete_all"
	logger.Log(context.Background(), LevelInfo, "user login", String("user_id", maliciousValue))

	out := buf.String()
	newlineCount := strings.Count(out, "\n")
	assert.Equal(t, 1, newlineCount,
		"CWE-117: field injection must not create extra log lines, got: %q", out)
}

func TestCWE117_FieldKeyInjection(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelInfo, "event",
		String("key\ninjected_key", "value"))

	out := buf.String()
	newlineCount := strings.Count(out, "\n")
	assert.Equal(t, 1, newlineCount,
		"CWE-117: field key injection must not create extra log lines")
}

func TestCWE117_GroupNameInjection(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := (&GoLogger{Level: LevelDebug}).
		WithGroup("safe\n[error] forged entry")

	logger.Log(context.Background(), LevelInfo, "test message")

	out := buf.String()
	newlineCount := strings.Count(out, "\n")
	assert.Equal(t, 1, newlineCount,
		"CWE-117: group name injection must not create extra log lines")
}

func TestCWE117_NullByteInjection(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelInfo, "before\x00after")

	out := buf.String()
	assert.NotContains(t, out, "\x00",
		"CWE-117: null bytes must not appear in log output")
}

func TestCWE117_ANSIEscapeSequences(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelInfo, "normal \x1b[31mRED ALERT\x1b[0m normal")

	out := buf.String()
	newlineCount := strings.Count(out, "\n")
	assert.Equal(t, 1, newlineCount,
		"ANSI escapes must not break single-line log output")
	assert.Contains(t, out, "normal")
}

func TestCWE117_TabInjection(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelInfo, "field1\tfield2\tfield3")

	out := buf.String()
	assert.NotContains(t, out, "\t",
		"tab characters should be escaped in log output")
	assert.Contains(t, out, `\t`)
}

func TestCWE117_MultipleVectorsSimultaneously(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	attack := "msg\n[error] fake\r[warn] also fake\ttab\x00null"
	logger.Log(context.Background(), LevelInfo, attack,
		String("user\nfake_key", "val\nfake_val"))

	out := buf.String()
	newlineCount := strings.Count(out, "\n")
	assert.Equal(t, 1, newlineCount,
		"CWE-117: combined attack must not create multiple log lines")
}

func TestCWE117_VeryLongMessageDoesNotCrash(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}

	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString(strings.Repeat("A", 1000))
		sb.WriteString("\n[error] forged entry ")
	}

	longMsg := sb.String()

	assert.NotPanics(t, func() {
		logger.Log(context.Background(), LevelInfo, longMsg)
	})

	out := buf.String()
	newlineCount := strings.Count(out, "\n")
	assert.Equal(t, 1, newlineCount,
		"CWE-117: very long message with injections must remain single-line")
}

// ===========================================================================
// GoLogger Behavioral Tests
// ===========================================================================

func TestGoLogger_OutputFormat(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelError, "something broke", String("code", "500"))

	out := buf.String()
	assert.Contains(t, out, "[error]")
	assert.Contains(t, out, "code=500")
	assert.Contains(t, out, "something broke")
}

func TestGoLogger_LevelFiltering(t *testing.T) {
	tests := []struct {
		name       string
		loggerLvl  Level
		msgLvl     Level
		shouldEmit bool
	}{
		{"error logger emits error", LevelError, LevelError, true},
		{"error logger suppresses warn", LevelError, LevelWarn, false},
		{"error logger suppresses info", LevelError, LevelInfo, false},
		{"error logger suppresses debug", LevelError, LevelDebug, false},
		{"warn logger emits error", LevelWarn, LevelError, true},
		{"warn logger emits warn", LevelWarn, LevelWarn, true},
		{"warn logger suppresses info", LevelWarn, LevelInfo, false},
		{"info logger emits info", LevelInfo, LevelInfo, true},
		{"info logger emits error", LevelInfo, LevelError, true},
		{"debug logger emits everything", LevelDebug, LevelDebug, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			withTestLoggerOutput(t, &buf)

			logger := &GoLogger{Level: tt.loggerLvl}
			logger.Log(context.Background(), tt.msgLvl, "test message")

			if tt.shouldEmit {
				assert.NotEmpty(t, buf.String(), "expected message to be emitted")
			} else {
				assert.Empty(t, buf.String(), "expected message to be suppressed")
			}
		})
	}
}

func TestGoLogger_WithPreservesFields(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := (&GoLogger{Level: LevelDebug}).
		With(String("service", "payments"), Int("version", 2))

	logger.Log(context.Background(), LevelInfo, "started")

	out := buf.String()
	assert.Contains(t, out, "service=payments")
	assert.Contains(t, out, "version=2")
}

func TestGoLogger_WithGroupNesting(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := (&GoLogger{Level: LevelDebug}).
		WithGroup("http").
		WithGroup("middleware")

	logger.Log(context.Background(), LevelInfo, "applied")

	out := buf.String()
	assert.Contains(t, out, "group=http.middleware")
}

func TestGoLogger_WithGroupEmptyNameIgnored(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := (&GoLogger{Level: LevelDebug}).
		WithGroup("").
		WithGroup("   ")

	logger.Log(context.Background(), LevelInfo, "test")

	out := buf.String()
	assert.NotContains(t, out, "group=")
}

func TestGoLogger_SyncReturnsNil(t *testing.T) {
	logger := &GoLogger{Level: LevelInfo}
	assert.NoError(t, logger.Sync(context.Background()))
}

func TestGoLogger_NilReceiverSafety(t *testing.T) {
	var logger *GoLogger

	assert.False(t, logger.Enabled(LevelError))

	assert.NotPanics(t, func() {
		child := logger.With(String("k", "v"))
		require.NotNil(t, child)
	})

	assert.NotPanics(t, func() {
		child := logger.WithGroup("grp")
		require.NotNil(t, child)
	})
}

func TestGoLogger_EmptyFieldKeySkipped(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelInfo, "msg", String("", "should_be_dropped"))

	out := buf.String()
	assert.NotContains(t, out, "should_be_dropped")
}

func TestGoLogger_BoolAndErrFields(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelInfo, "event",
		Bool("active", true),
		Err(assert.AnError))

	out := buf.String()
	assert.Contains(t, out, "active=true")
	assert.Contains(t, out, "error=")
}

func TestGoLogger_AnyFieldConstructor(t *testing.T) {
	f := Any("data", map[string]int{"count": 42})
	assert.Equal(t, "data", f.Key)
	assert.NotNil(t, f.Value)
}

func TestGoLogger_SensitiveFieldRedaction(t *testing.T) {
	var buf bytes.Buffer
	withTestLoggerOutput(t, &buf)

	logger := &GoLogger{Level: LevelDebug}
	logger.Log(context.Background(), LevelInfo, "login attempt",
		String("password", "super_secret"),
		String("api_key", "key-12345"),
		String("user_id", "u-42"),
	)

	out := buf.String()
	assert.NotContains(t, out, "super_secret", "password value must be redacted")
	assert.NotContains(t, out, "key-12345", "api_key value must be redacted")
	assert.Contains(t, out, "[REDACTED]", "redacted fields must show [REDACTED]")
	assert.Contains(t, out, "user_id=u-42", "non-sensitive fields must pass through")
}

func TestGoLogger_WithGroupEmptyReturnsReceiver(t *testing.T) {
	logger := &GoLogger{Level: LevelDebug}
	same := logger.WithGroup("")
	assert.Same(t, logger, same, "WithGroup(\"\") should return the same logger")
}

func TestParseLevel_WhitespaceTrimming(t *testing.T) {
	tests := []struct {
		input    string
		expected Level
	}{
		{" debug ", LevelDebug},
		{"\tinfo\n", LevelInfo},
		{"  warn  ", LevelWarn},
		{"\nerror\t", LevelError},
	}

	for _, tt := range tests {
		level, err := ParseLevel(tt.input)
		require.NoError(t, err, "ParseLevel(%q) should not error", tt.input)
		assert.Equal(t, tt.expected, level)
	}
}

// ===========================================================================
// NopLogger Comprehensive Tests
// ===========================================================================

func TestNopLogger_AllMethodsAreNoOps(t *testing.T) {
	nop := NewNop()

	t.Run("Log does not panic at any level", func(t *testing.T) {
		assert.NotPanics(t, func() {
			for _, level := range []Level{LevelError, LevelWarn, LevelInfo, LevelDebug} {
				nop.Log(context.Background(), level, "message",
					String("k", "v"), Int("n", 1), Bool("b", true))
			}
		})
	})

	t.Run("With returns self", func(t *testing.T) {
		child := nop.With(String("a", "b"), String("c", "d"))
		assert.Equal(t, nop, child)
	})

	t.Run("WithGroup returns self", func(t *testing.T) {
		child := nop.WithGroup("any_group")
		assert.Equal(t, nop, child)
	})

	t.Run("Enabled always false", func(t *testing.T) {
		for _, level := range []Level{LevelError, LevelWarn, LevelInfo, LevelDebug} {
			assert.False(t, nop.Enabled(level))
		}
	})

	t.Run("Sync returns nil", func(t *testing.T) {
		assert.NoError(t, nop.Sync(context.Background()))
	})
}

func TestNopLogger_InterfaceCompliance(t *testing.T) {
	var _ Logger = NewNop()
	var _ Logger = &NopLogger{}
}

// ===========================================================================
// MockLogger Verification Tests
// ===========================================================================

func TestMockLogger_RecordsCalls(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockLogger(ctrl)

	ctx := context.Background()

	mock.EXPECT().Enabled(LevelInfo).Return(true)
	mock.EXPECT().Log(ctx, LevelInfo, "hello", String("k", "v"))
	mock.EXPECT().Sync(ctx).Return(nil)

	assert.True(t, mock.Enabled(LevelInfo))
	mock.Log(ctx, LevelInfo, "hello", String("k", "v"))
	assert.NoError(t, mock.Sync(ctx))
}

func TestMockLogger_WithAndWithGroup(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockLogger(ctrl)

	childMock := NewMockLogger(ctrl)

	mock.EXPECT().With(String("tenant", "t1")).Return(childMock)
	mock.EXPECT().WithGroup("audit").Return(childMock)

	child1 := mock.With(String("tenant", "t1"))
	assert.Equal(t, childMock, child1)

	child2 := mock.WithGroup("audit")
	assert.Equal(t, childMock, child2)
}

func TestMockLogger_InterfaceCompliance(t *testing.T) {
	ctrl := gomock.NewController(t)
	var _ Logger = NewMockLogger(ctrl)
}

// ===========================================================================
// Level String Tests
// ===========================================================================

func TestLevel_String(t *testing.T) {
	tests := []struct {
		level    Level
		expected string
	}{
		{LevelError, "error"},
		{LevelWarn, "warn"},
		{LevelInfo, "info"},
		{LevelDebug, "debug"},
		{Level(255), "unknown"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, tt.level.String())
	}
}

// ===========================================================================
// renderFields Tests
// ===========================================================================

func TestRenderFields_EmptyReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", renderFields(nil))
	assert.Equal(t, "", renderFields([]Field{}))
}

func TestRenderFields_SingleField(t *testing.T) {
	result := renderFields([]Field{String("status", "ok")})
	assert.Equal(t, "[status=ok]", result)
}

func TestRenderFields_MultipleFields(t *testing.T) {
	result := renderFields([]Field{
		String("a", "1"),
		Int("b", 2),
		Bool("c", true),
	})
	assert.Contains(t, result, "a=1")
	assert.Contains(t, result, "b=2")
	assert.Contains(t, result, "c=true")
}

func TestRenderFields_EmptyKeyFieldSkipped(t *testing.T) {
	result := renderFields([]Field{String("", "val")})
	assert.Equal(t, "", result)
}

func TestRenderFields_SanitizesKeysAndValues(t *testing.T) {
	result := renderFields([]Field{
		String("status\ninjection", "value\ninjection"),
	})
	assert.NotContains(t, result, "\n")
	assert.Contains(t, result, `\n`)
}

// ===========================================================================
// sanitizeFieldValue Tests
// ===========================================================================

type testStringer struct{ s string }

func (ts testStringer) String() string { return ts.s }

func TestSanitizeFieldValue(t *testing.T) {
	tests := []struct {
		name     string
		input    any
		expected any
	}{
		{
			name:     "plain string passthrough",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "string with newline is sanitized",
			input:    "line1\nline2",
			expected: `line1\nline2`,
		},
		{
			name:     "error with newline is sanitized",
			input:    errors.New("bad\ninput"),
			expected: `bad\ninput`,
		},
		{
			name:     "fmt.Stringer with newline is sanitized",
			input:    testStringer{s: "hello\nworld"},
			expected: `hello\nworld`,
		},
		{
			name:     "integer passes through unchanged",
			input:    42,
			expected: 42,
		},
		{
			name:     "nil passes through unchanged",
			input:    nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeFieldValue(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
