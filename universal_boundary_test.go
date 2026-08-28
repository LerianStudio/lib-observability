//go:build unit

package observability_test

// TestUniversalBoundary is the executable statement of what v4 buys.
//
// # The defect this module's v4 exists to eliminate
//
// Go matches the types inside a function or method signature NOMINALLY. A
// parameter declared as log.Level or log.Field binds the caller to THIS
// module's version of that type: v3/log.Level and v4/log.Level are different
// types even when the source is byte-for-byte identical. Any consumer that
// wanted to hand us a logger therefore had to import us, and importing us,
// inherited our major version. One Fiber upgrade inside middleware/ then
// rewrote ~875 files across the fleet that had nothing to do with Fiber.
//
// # The proof
//
// Everything in this file is built from LOCAL types - declared right here,
// implementing nothing named by lib-observability - and handed to this
// module's exported functions with NO adapter and NO wrapper call. Every one
// of these calls compiling is the guarantee. The runtime assertions on top of
// that prove the foreign value is actually driven, not merely accepted and
// discarded.
//
// If any of this file stops compiling, a defined type has reappeared in a
// parameter position that accepts a logger or a recorder, and the fleet-wide
// major-version propagation is back.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	observability "github.com/LerianStudio/lib-observability/v4"
	obsassert "github.com/LerianStudio/lib-observability/v4/assert"
	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/metrics"
	"github.com/LerianStudio/lib-observability/v4/middleware"
	obsruntime "github.com/LerianStudio/lib-observability/v4/runtime"
	obszap "github.com/LerianStudio/lib-observability/v4/zap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

// ===========================================================================
// The foreign types. Note what is NOT here: no import of any lib-observability
// type in these declarations, no embedding, no `var _ log.Logger = ...`.
// Every parameter is a universal Go type - context.Context, int, string, any,
// map[string]string, int64, float64, []float64, error.
// ===========================================================================

// localLogger is what a consumer would write in its own package. It satisfies
// log.Universal, metrics.Logger, runtime.Logger and assert.Logger purely
// STRUCTURALLY - it has never heard of any of them.
type localLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *localLogger) Log(_ context.Context, level int, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = append(l.entries, fmt.Sprintf("level=%d msg=%q fields=%d", level, msg, len(fields)))
}

func (l *localLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.entries)
}

func (l *localLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return fmt.Sprint(l.entries)
}

// localRecorder is the metrics half of the same story: one method, universal
// types, satisfying runtime.Recorder and assert.Recorder structurally.
type localRecorder struct {
	mu    sync.Mutex
	names []string
	attrs []map[string]string
	err   error
}

func (r *localRecorder) AddCounter(
	_ context.Context,
	name, _, _ string,
	attrs map[string]string,
	_ int64,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.names = append(r.names, name)
	r.attrs = append(r.attrs, attrs)

	return r.err
}

func (r *localRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.names))
	copy(out, r.names)

	return out
}

// localFullRecorder carries the whole three-method recording surface, again
// declared locally. It is the shape a consumer needs to accept a metrics
// factory from ANY version of this module.
type localFullRecorder interface {
	AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error
	SetGauge(ctx context.Context, name, description, unit string, attrs map[string]string, value int64) error
	RecordHistogram(
		ctx context.Context,
		name, description, unit string,
		attrs map[string]string,
		value float64,
		buckets []float64,
	) error
}

// localLoggerInterface is the reverse direction: the one-method interface a
// consumer declares to ACCEPT a logger from this module.
type localLoggerInterface interface {
	Log(ctx context.Context, level int, msg string, fields ...any)
}

// localSystemRecorder is the system-readings surface, declared locally.
type localSystemRecorder struct {
	mu   sync.Mutex
	cpu  []int64
	mem  []int64
	fail error
}

func (r *localSystemRecorder) RecordSystemCPUUsage(_ context.Context, percentage int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cpu = append(r.cpu, percentage)

	return r.fail
}

func (r *localSystemRecorder) RecordSystemMemUsage(_ context.Context, percentage int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mem = append(r.mem, percentage)

	return r.fail
}

// ===========================================================================
// Outbound: a foreign type goes INTO this module, with no adapter.
// ===========================================================================

func TestForeignLogger_RuntimeSafeGo(t *testing.T) {
	logger := &localLogger{}
	done := make(chan struct{})

	// No adapter. No wrapper. The local type goes straight in.
	obsruntime.SafeGo(logger, "foreign-goroutine", obsruntime.KeepRunning, func() {
		defer close(done)
		panic("boom from a goroutine")
	})

	<-done

	require.Eventually(t, func() bool { return logger.count() > 0 }, time.Second, time.Millisecond)
	assert.Contains(t, logger.joined(), "panic")
}

func TestForeignLogger_RuntimeRecoverAndLog(t *testing.T) {
	logger := &localLogger{}

	func() {
		defer obsruntime.RecoverAndLog(logger, "foreign-recover")
		panic("boom")
	}()

	assert.Positive(t, logger.count(), "the foreign logger must actually be driven")
}

func TestForeignRecorder_RuntimeInitPanicMetrics(t *testing.T) {
	obsruntime.ResetPanicMetrics()
	t.Cleanup(obsruntime.ResetPanicMetrics)

	recorder := &localRecorder{}
	logger := &localLogger{}

	// Both foreign values, both accepted directly.
	obsruntime.InitPanicMetrics(recorder, logger)
	require.NotNil(t, obsruntime.GetPanicMetrics())

	func() {
		defer obsruntime.RecoverAndLogWithContext(context.Background(), logger, "ledger", "handler")
		panic("boom")
	}()

	assert.Equal(t, []string{"panic_recovered_total"}, recorder.recorded())
	assert.Positive(t, logger.count())
}

func TestForeignLogger_AssertNew(t *testing.T) {
	logger := &localLogger{}

	// No adapter: the local logger IS the parameter.
	asserter := obsassert.New(context.Background(), logger, "component", "operation")

	err := asserter.That(context.Background(), false, "invariant violated")
	require.Error(t, err)
	assert.Positive(t, logger.count(), "the foreign logger must receive the assertion failure")
}

func TestForeignRecorder_AssertInitAssertionMetrics(t *testing.T) {
	obsassert.ResetAssertionMetrics()
	t.Cleanup(obsassert.ResetAssertionMetrics)

	recorder := &localRecorder{}
	obsassert.InitAssertionMetrics(recorder)
	require.NotNil(t, obsassert.GetAssertionMetrics())

	asserter := obsassert.New(context.Background(), &localLogger{}, "ledger", "post")

	_ = asserter.Never(context.Background(), "unreachable")

	obsassert.ResetAssertionMetrics()

	require.Equal(t, []string{"assertion_failed_total"}, recorder.recorded())
	require.Len(t, recorder.attrs, 1)
	assert.Equal(t, "ledger", recorder.attrs[0]["component"])
}

func TestForeignLogger_MetricsNewMetricsFactory(t *testing.T) {
	t.Parallel()

	logger := &localLogger{}

	factory, err := metrics.NewMetricsFactory(noop.NewMeterProvider().Meter("foreign"), logger)
	require.NoError(t, err)
	require.NotNil(t, factory)

	assert.NoError(t, factory.AddCounter(context.Background(), "c", "d", "1", nil, 1))
}

func TestForeignLogger_ObservabilityContextWithLogger(t *testing.T) {
	t.Parallel()

	logger := &localLogger{}

	ctx := observability.ContextWithLogger(context.Background(), logger)

	// The logger comes back out as a full log.Logger - conversion happened
	// inside the module, at the call, not at the consumer.
	resolved := observability.NewLoggerFromContext(ctx)
	require.NotNil(t, resolved)

	resolved.Log(ctx, 2, "round trip")
	assert.Equal(t, 1, logger.count(), "the entry must reach the foreign logger, not a black hole")
}

func TestForeignLogger_LogSafeError(t *testing.T) {
	t.Parallel()

	logger := &localLogger{}

	log.SafeError(logger, context.Background(), "failed", errors.New("cause"), false)

	assert.Equal(t, 1, logger.count())
}

func TestForeignLogger_MiddlewareWithCustomLogger(t *testing.T) {
	t.Parallel()

	logger := &localLogger{}

	// Compiling at all is the assertion: the middleware package is where the
	// Fiber major lives, and it must still accept a logger from a package that
	// has never imported lib-observability.
	option := middleware.WithCustomLogger(logger)
	assert.NotNil(t, option)
}

func TestForeignLogger_ZapSlog(t *testing.T) {
	t.Parallel()

	logger := &localLogger{}

	slogger := obszap.Slog(logger)
	require.NotNil(t, slogger)

	// A non-zap logger falls back to a discarding handler rather than
	// panicking - "an unknown logger is silent", never fatal.
	assert.NotPanics(t, func() { slogger.Info("discarded") })
}

func TestForeignSystemRecorder_ObservabilitySystemReadings(t *testing.T) {
	t.Parallel()

	recorder := &localSystemRecorder{}
	ctx := context.Background()

	observability.GetCPUUsage(ctx, recorder)
	observability.GetMemUsage(ctx, recorder)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	assert.Len(t, recorder.cpu, 1)
	assert.Len(t, recorder.mem, 1)
}

// ===========================================================================
// Inbound: a value from this module satisfies a LOCALLY declared interface.
// ===========================================================================

// TestModuleLoggersSatisfyLocalInterface is the half that lets a consumer
// ACCEPT a logger without importing us. The assignments below are the whole
// test - they are checked by the compiler.
func TestModuleLoggersSatisfyLocalInterface(t *testing.T) {
	t.Parallel()

	var (
		goLogger  localLoggerInterface = &log.GoLogger{Level: log.LevelDebug}
		nopLogger localLoggerInterface = log.NewNop()
	)

	require.NotNil(t, goLogger)
	require.NotNil(t, nopLogger)

	// And the tracking bundle's logger, which is what service code actually
	// holds after NewTrackingFromContext.
	tracked, _, _, factory := observability.NewTrackingFromContext(context.Background())

	var fromTracking localLoggerInterface = tracked

	require.NotNil(t, fromTracking)

	assert.NotPanics(t, func() {
		goLogger.Log(context.Background(), log.LevelInfo, "ok")
		nopLogger.Log(context.Background(), log.LevelInfo, "ok")
		fromTracking.Log(context.Background(), log.LevelInfo, "ok")
	})

	// Same for the metrics factory against the locally declared recorder shape.
	var localRec localFullRecorder = factory

	require.NotNil(t, localRec)

	ctx := context.Background()
	assert.NoError(t, localRec.AddCounter(ctx, "c", "d", "1", nil, 1))
	assert.NoError(t, localRec.SetGauge(ctx, "g", "d", "1", nil, 1))
	assert.NoError(t, localRec.RecordHistogram(ctx, "h", "d", "ms", nil, 1, nil))
}

// TestForeignLoggerIsNotWrappedUnnecessarily records the other half of the
// contract: a value that is ALREADY a full log.Logger is handed back by Adapt
// unchanged, so its native semantics survive the universal boundary. Only a
// Log-only logger - like localLogger - gets a shim.
func TestForeignLoggerIsNotWrappedUnnecessarily(t *testing.T) {
	t.Parallel()

	native := &log.GoLogger{Level: log.LevelDebug}
	ctx := observability.ContextWithLogger(context.Background(), native)

	assert.Same(t, native, observability.NewLoggerFromContext(ctx))

	foreign := &localLogger{}
	foreignCtx := observability.ContextWithLogger(context.Background(), foreign)

	assert.NotSame(t, any(foreign), any(observability.NewLoggerFromContext(foreignCtx)))
}
