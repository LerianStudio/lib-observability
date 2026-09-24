//go:build unit

package log

import (
	"context"
	"fmt"
	"slices"
	"testing"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/LerianStudio/lib-observability/v4/internal/panicobs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// secret is what a consumer might log or panic with; no report may carry it.
const secret = "4111-1111-1111-1111"

// panickingUniversal is a Log-only logger with its own level check that
// panics with the secret wherever it is told to.
type panickingUniversal struct {
	inLog, inEnabled bool
}

func (p *panickingUniversal) Log(context.Context, int, string, ...any) {
	if p.inLog {
		panic(secret)
	}
}

func (p *panickingUniversal) Enabled(int) bool {
	if p.inEnabled {
		panic(fmt.Errorf("level check: %s", secret))
	}

	return true
}

// captureFallback swaps the fallback logger for a recorder for one test. The
// callers are sequential tests, so no parallel test observes the swap.
func captureFallback(t *testing.T) *recordingUniversal {
	t.Helper()

	sink := &recordingUniversal{}
	previous := panicFallback
	panicFallback = sink

	t.Cleanup(func() { panicFallback = previous })

	return sink
}

// guardCases lists every shim method that runs consumer code. With, WithGroup
// and Sync never call into the wrapped logger, so nothing there can panic.
func guardCases() []struct {
	name, panicType string
	call            func(context.Context, *testing.T)
} {
	return []struct {
		name, panicType string
		call            func(context.Context, *testing.T)
	}{
		{name: "Log", panicType: "string", call: func(ctx context.Context, _ *testing.T) {
			Adapt(&panickingUniversal{inLog: true}).
				With(String("bound", secret)).
				Log(ctx, LevelInfo, "charge "+secret, String("card", secret))
		}},
		{name: "Enabled", panicType: "*errors.errorString", call: func(_ context.Context, t *testing.T) {
			assert.True(t, Adapt(&panickingUniversal{inEnabled: true}).Enabled(LevelInfo),
				"a level check that panicked answers true, the answer that cannot drop an entry")
		}},
	}
}

// TestAdapt_PanicInsideLoggerIsRecoveredAndReported: a consumer logger that
// panics must never unwind the caller, and the one ERROR line reporting it
// goes to a logger that is not the panicking one, naming the method and the
// panic value's type, never the value, the message or the fields.
func TestAdapt_PanicInsideLoggerIsRecoveredAndReported(t *testing.T) {
	for _, tc := range guardCases() {
		t.Run(tc.name, func(t *testing.T) {
			sink := captureFallback(t)

			require.NotPanics(t, func() { tc.call(context.Background(), t) })

			require.Len(t, sink.entries, 1)
			entry := sink.entries[0]
			assert.Equal(t, LevelError, entry.level)
			assert.Equal(t, loggerPanicMsg, entry.msg)
			assert.Contains(t, entry.fields, String("method", tc.name))
			assert.Contains(t, entry.fields, String("panic_type", tc.panicType))
			assert.NotContains(t, fmt.Sprint(entry), secret)
		})
	}
}

// TestAdapt_PanicReportCarriesStackOutsideProductionOnly mirrors runtime's
// recovered-panic log line: the stack trace outside production mode, none in it.
func TestAdapt_PanicReportCarriesStackOutsideProductionOnly(t *testing.T) {
	initial := panicobs.ProductionMode()
	t.Cleanup(func() { panicobs.SetProductionMode(initial) })

	for _, production := range []bool{false, true} {
		t.Run(fmt.Sprintf("production=%t", production), func(t *testing.T) {
			panicobs.SetProductionMode(production)

			sink := captureFallback(t)
			Adapt(&panickingUniversal{inLog: true}).Log(context.Background(), LevelInfo, "m")

			require.Len(t, sink.entries, 1)
			assert.Equal(t, !production, slices.Contains(fieldKeys(sink.entries[0].fields), "stack_trace"))
		})
	}
}

// TestAdapt_PanicWithNilContextIsRecovered: a nil ctx is the caller's bug, but
// it must not turn a recovered panic into a second one.
func TestAdapt_PanicWithNilContextIsRecovered(t *testing.T) {
	sink := captureFallback(t)

	require.NotPanics(t, func() {
		//nolint:staticcheck // a nil ctx is exactly the case under test.
		Adapt(&panickingUniversal{inLog: true}).Log(nil, LevelInfo, "m")
	})
	assert.Len(t, sink.entries, 1)
}

// TestAdapt_DefaultFallbackIsThisPackagesStdlibLogger: out of the box the
// report lands on a GoLogger at ERROR, never on the consumer's logger.
func TestAdapt_DefaultFallbackIsThisPackagesStdlibLogger(t *testing.T) {
	fallback, ok := panicFallback.(*GoLogger)
	require.True(t, ok)
	assert.True(t, fallback.Enabled(LevelError))
}

// TestAdapt_PanicInsideLoggerRecordsSpanEvent: a Log call carries a ctx, so the
// panic lands on its span exactly as runtime records one - a panic.recovered
// event, an error and an Error status - valued by the panic's type only.
func TestAdapt_PanicInsideLoggerRecordsSpanEvent(t *testing.T) {
	captureFallback(t)

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "op")
	Adapt(&panickingUniversal{inLog: true}).Log(ctx, LevelInfo, "charge "+secret, String("card", secret))
	span.End()

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)

	var event *sdktrace.Event

	for i := range spans[0].Events() {
		if spans[0].Events()[i].Name == constant.EventPanicRecovered {
			event = &spans[0].Events()[i]
		}
	}

	require.NotNil(t, event, "the panic.recovered event must be on the caller's span")
	assert.Contains(t, event.Attributes, attribute.String("panic.value", "string"))
	assert.Contains(t, event.Attributes, attribute.String("panic.goroutine_name", "Log"))
	assert.Contains(t, event.Attributes, attribute.String("panic.component", "log"))
	assert.NotContains(t, fmt.Sprint(spans[0].Events()), secret)
}

// TestAdapt_PanickingFallbackStillRecordsSpanEvent: the fallback line runs on
// the stdlib logger, which a host may redirect into the very logger that just
// panicked; the span event must not depend on that line surviving.
func TestAdapt_PanickingFallbackStillRecordsSpanEvent(t *testing.T) {
	previous := panicFallback
	panicFallback = &panickingUniversal{inLog: true}

	t.Cleanup(func() { panicFallback = previous })

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "op")
	require.NotPanics(t, func() { Adapt(&panickingUniversal{inLog: true}).Log(ctx, LevelInfo, "m") })
	span.End()

	require.Len(t, recorder.Ended(), 1)
	assert.True(t, slices.ContainsFunc(recorder.Ended()[0].Events(), func(e sdktrace.Event) bool {
		return e.Name == constant.EventPanicRecovered
	}))
}

// discardUniversal is a Log-only logger that keeps nothing.
type discardUniversal struct{}

func (discardUniversal) Log(context.Context, int, string, ...any) {}

func BenchmarkAdaptedShimLog(b *testing.B) {
	logger := Adapt(discardUniversal{})
	ctx := context.Background()

	b.ReportAllocs()

	for b.Loop() {
		logger.Log(ctx, LevelInfo, "m")
	}
}
