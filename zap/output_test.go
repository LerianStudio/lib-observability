//go:build unit

package zap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	logpkg "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"
)

// swapStderr redirects the process stderr to a pipe for the duration of the
// test and returns a reader closure yielding everything written to it.
// The original stderr is restored on cleanup.
func swapStderr(t *testing.T) func() string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	orig := os.Stderr
	os.Stderr = w

	t.Cleanup(func() {
		os.Stderr = orig
		_ = w.Close()
		_ = r.Close()
	})

	return func() string {
		// Closing the write end unblocks ReadAll at EOF. Test volumes are far
		// below the pipe buffer, so no draining goroutine is needed.
		_ = w.Close()

		out, err := io.ReadAll(r)
		require.NoError(t, err)

		return string(out)
	}
}

func newOutputLogger(t *testing.T, w io.Writer, level string) *Logger {
	t.Helper()

	logger, err := New(Config{
		Environment:     EnvironmentProduction,
		Level:           level,
		OTelLibraryName: "svc",
		Output:          w,
	})
	require.NoError(t, err)

	return logger
}

func TestOutputWritesOneEntryToWriterAndNothingToStderr(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		check    func(t *testing.T, out string)
	}{
		{
			name:     "json",
			encoding: "json",
			check: func(t *testing.T, out string) {
				t.Helper()

				var entry map[string]any
				require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &entry))
				assert.Equal(t, "hello", entry["msg"])
				assert.Equal(t, "INFO", entry["level"])
				assert.Equal(t, "req-1", entry["request_id"])
			},
		},
		{
			name:     "console",
			encoding: "console",
			check: func(t *testing.T, out string) {
				t.Helper()

				assert.NotContains(t, out, `"msg":`, "console encoding must not emit JSON")
				assert.Contains(t, out, "INFO")
				assert.Contains(t, out, "hello")
				assert.Contains(t, out, "req-1")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LOG_ENCODING", tt.encoding)

			readStderr := swapStderr(t)

			var buf bytes.Buffer

			logger := newOutputLogger(t, &buf, "info")

			logger.Log(context.Background(), logpkg.LevelInfo, "hello", logpkg.String("request_id", "req-1"))
			require.NoError(t, logger.Sync(context.Background()))

			trimmed := strings.TrimSpace(buf.String())
			require.NotEmpty(t, trimmed)
			assert.Equal(t, 0, strings.Count(trimmed, "\n"), "exactly one log line expected")

			tt.check(t, trimmed)

			assert.Empty(t, readStderr(), "no log entry should reach the process stderr")
		})
	}
}

func TestOutputCarriesTraceCorrelation(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")

	traceID, err := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	require.NoError(t, err)

	spanID, err := trace.SpanIDFromHex("b7ad6b7169203331")
	require.NoError(t, err)

	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	var buf bytes.Buffer

	logger := newOutputLogger(t, &buf, "info")
	logger.Log(ctx, logpkg.LevelInfo, "traced message")

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry))
	assert.Equal(t, traceID.String(), entry["trace_id"])
	assert.Equal(t, spanID.String(), entry["span_id"])
}

func TestOutputRespectsLevel(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")

	var buf bytes.Buffer

	logger := newOutputLogger(t, &buf, "info")
	require.Equal(t, zapcore.InfoLevel, logger.Level().Level())

	logger.Log(context.Background(), logpkg.LevelDebug, "debug dropped")
	assert.Empty(t, buf.String(), "debug must be filtered out at info level")

	logger.Log(context.Background(), logpkg.LevelInfo, "info kept")
	assert.Contains(t, buf.String(), "info kept")

	// The runtime handle must still drive the writer core.
	logger.Level().SetLevel(zapcore.DebugLevel)
	logger.Log(context.Background(), logpkg.LevelDebug, "debug now kept")
	assert.Contains(t, buf.String(), "debug now kept")
}

func TestOutputSyncsFileWriter(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")

	f, err := os.CreateTemp(t.TempDir(), "log-*.json")
	require.NoError(t, err)

	t.Cleanup(func() { _ = f.Close() })

	logger := newOutputLogger(t, f, "info")
	logger.Log(context.Background(), logpkg.LevelInfo, "to file")
	require.NoError(t, logger.Sync(context.Background()))

	contents, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	assert.Contains(t, string(contents), "to file")
}

func TestNilOutputStillWritesToStderr(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")

	readStderr := swapStderr(t)

	logger, err := New(Config{
		Environment:     EnvironmentProduction,
		Level:           "info",
		OTelLibraryName: "svc",
	})
	require.NoError(t, err)

	logger.Log(context.Background(), logpkg.LevelInfo, "to stderr")

	assert.Contains(t, readStderr(), "to stderr")
}

func TestSlogWritesThroughOutputCore(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")

	var buf bytes.Buffer

	Slog(newOutputLogger(t, &buf, "info")).Info("via slog", "request_id", "req-2")

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry))
	assert.Equal(t, "via slog", entry["msg"])
	assert.Equal(t, "req-2", entry["request_id"])
}
