//go:build unit

package log

import (
	"context"
	"errors"
	"fmt"
	"testing"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var errSyncFailed = errors.New("sync failed")

// panickingFull is a consumer's own full Logger that panics in one method.
type panickingFull struct {
	in string
}

func (p *panickingFull) boom(method string) {
	if p.in == method {
		panic(fmt.Errorf("%s: %s", method, secret))
	}
}

func (p *panickingFull) Log(context.Context, int, string, ...any) { p.boom("Log") }

//nolint:ireturn
func (p *panickingFull) With(...any) Logger { p.boom("With"); return p }

//nolint:ireturn
func (p *panickingFull) WithGroup(string) Logger { p.boom("WithGroup"); return p }

func (p *panickingFull) Enabled(int) bool { p.boom("Enabled"); return false }

func (p *panickingFull) Sync(context.Context) error { p.boom("Sync"); return nil }

// TestGuard_PanicInEveryMethodIsRecoveredAndReported: whichever method of a
// guarded logger panics, the caller does not unwind, gets a usable answer, and
// one ERROR line names the method and the panic's type, never the secret.
func TestGuard_PanicInEveryMethodIsRecoveredAndReported(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		method string
		call   func(*testing.T, Logger)
	}{
		{method: "Log", call: func(_ *testing.T, l Logger) {
			l.Log(ctx, LevelInfo, "charge "+secret, String("card", secret))
		}},
		{method: "With", call: func(t *testing.T, l Logger) {
			assert.Same(t, l, l.With(String("card", secret)), "a panicking With keeps the receiver")
		}},
		{method: "WithGroup", call: func(t *testing.T, l Logger) {
			assert.Same(t, l, l.WithGroup(secret), "a panicking WithGroup keeps the receiver")
		}},
		{method: "Enabled", call: func(t *testing.T, l Logger) {
			assert.True(t, l.Enabled(LevelInfo), "a panicking level check answers true for a defined level")
			assert.False(t, l.Enabled(LevelUnknown), "and false for an undefined one")
		}},
		{method: "Sync", call: func(t *testing.T, l Logger) {
			assert.ErrorIs(t, l.Sync(ctx), errLoggerPanicked)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			sink := captureFallback(t)
			guarded := Guard(&panickingFull{in: tt.method})

			require.NotPanics(t, func() { tt.call(t, guarded) })

			require.NotEmpty(t, sink.entries)
			entry := sink.entries[0]
			assert.Equal(t, LevelError, entry.level)
			assert.Equal(t, loggerPanicMsg, entry.msg)
			assert.Contains(t, entry.fields, String("method", tt.method))
			assert.Contains(t, entry.fields, String("panic_type", "*errors.errorString"))
			assert.NotContains(t, fmt.Sprint(sink.entries), secret)
		})
	}
}

func TestGuard_PanicInLogRecordsSpanEvent(t *testing.T) {
	captureFallback(t)

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "op")
	Guard(&panickingFull{in: "Log"}).Log(ctx, LevelInfo, "m", String("card", secret))
	span.End()

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	require.Len(t, spans[0].Events(), 2, "the panic.recovered event plus RecordError's exception event")
	assert.Equal(t, constant.EventPanicRecovered, spans[0].Events()[0].Name)
	assert.Contains(t, spans[0].Events()[0].Attributes, attribute.String("panic.value", "*errors.errorString"))
	assert.Contains(t, spans[0].Events()[0].Attributes, attribute.String("panic.goroutine_name", "Log"))
	assert.NotContains(t, fmt.Sprint(spans[0].Events()), secret)
}

// deriving is a full Logger that never panics itself but hands out children
// that do, so only a guard that survives With/WithGroup catches them.
type deriving struct{ recordingUniversal }

//nolint:ireturn
func (d *deriving) With(...any) Logger { return &panickingFull{in: "Log"} }

//nolint:ireturn
func (d *deriving) WithGroup(string) Logger { return &panickingFull{in: "Log"} }

func (d *deriving) Enabled(int) bool { return true }

func (d *deriving) Sync(context.Context) error { return nil }

func TestGuard_DerivedLoggersStayGuarded(t *testing.T) {
	captureFallback(t)

	guarded := Guard(&deriving{})

	for name, child := range map[string]Logger{
		"With":      guarded.With(String("k", "v")),
		"WithGroup": guarded.WithGroup("g"),
	} {
		assert.NotPanics(t, func() { child.Log(context.Background(), LevelInfo, "m") }, name)
	}
}

// recordingFull is a full Logger that never panics: the guard around it must
// be invisible.
type recordingFull struct {
	recordingUniversal
	level   int
	syncErr error
}

//nolint:ireturn
func (r *recordingFull) With(...any) Logger { return r }

//nolint:ireturn
func (r *recordingFull) WithGroup(string) Logger { return r }

func (r *recordingFull) Enabled(level int) bool { return level <= r.level }

func (r *recordingFull) Sync(context.Context) error { return r.syncErr }

func TestGuard_IsInvisibleForALoggerThatNeverPanics(t *testing.T) {
	sink := captureFallback(t)

	full := &recordingFull{level: LevelInfo, syncErr: errSyncFailed}
	guarded := Guard(full)

	guarded.Log(context.Background(), LevelWarn, "hello", String("k", "v"))
	assert.Equal(t, universalEntry{level: LevelWarn, msg: "hello", fields: []Field{{Key: "k", Value: "v"}}}, full.last(t))
	assert.True(t, guarded.Enabled(LevelInfo))
	assert.False(t, guarded.Enabled(LevelDebug))
	require.ErrorIs(t, guarded.Sync(context.Background()), errSyncFailed)
	assert.Empty(t, sink.entries)
}

func TestGuard_Passthrough(t *testing.T) {
	t.Parallel()

	goLogger := &GoLogger{Level: LevelInfo}
	nop := NewNop()
	shim := Adapt(&recordingUniversal{})
	guarded := Guard(&recordingFull{})

	assert.Same(t, goLogger, Guard(goLogger), "this package's stderr logger never panics on its own")
	assert.Same(t, nop, Guard(nop))
	assert.Same(t, shim, Guard(shim), "the Adapt shim already guards the only methods that run consumer code")
	assert.Same(t, guarded, Guard(guarded), "guarding twice returns the same guard")
	assert.IsType(t, &universalShim{}, Guard(&recordingUniversal{}), "a Log-only logger gets the Adapt shim, already guarded")
}

func TestGuard_NilReturnsNop(t *testing.T) {
	t.Parallel()

	var typedNil *recordingFull

	assert.IsType(t, &NopLogger{}, Guard(nil))
	assert.IsType(t, &NopLogger{}, Guard(typedNil))
}

// foreignNop is a no-op logger Guard does not recognize, so it gets wrapped.
type foreignNop struct{ NopLogger }

func BenchmarkGuardedLog(b *testing.B) {
	logger := Guard(&foreignNop{})
	ctx := context.Background()

	b.ReportAllocs()

	for b.Loop() {
		logger.Log(ctx, LevelInfo, "m")
	}
}
