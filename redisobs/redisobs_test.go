//go:build unit

package redisobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// poolStatMetric is one of the ASYNCHRONOUS pool-stat instruments redisotel
// registers on the meter. Asserted by name so the test fails loudly if the
// upstream namespace ever drifts.
const poolStatMetric = "db.client.connections.usage"

// newRedisHarness builds real OTel SDK providers plus an in-memory span exporter
// so the test can assert on the spans redisotel produces without an external
// backend.
func newRedisHarness(t *testing.T) (*sdkmetric.MeterProvider, *sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()

	mp := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	return mp, tp, spanExp
}

// newUnreachableClient returns a go-redis UniversalClient pointed at an address
// that will fail to dial fast. redisotel's ProcessHook still creates the command
// span before the dial is attempted, so span attributes are observable without a
// live server.
func newUnreachableClient(t *testing.T) redis.UniversalClient {
	t.Helper()

	c := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:       []string{"127.0.0.1:1"}, // nothing listens here
		DialTimeout: 50 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = c.Close() })

	return c
}

// TestInstrument_AppliesHooksAndCreatesSpan verifies the helper wires tracing so
// a command produces a redis span, and does so without error.
func TestInstrument_AppliesHooksAndCreatesSpan(t *testing.T) {
	mp, tp, spanExp := newRedisHarness(t)

	client := newUnreachableClient(t)

	err := Instrument(client, WithMeterProvider(mp), WithTracerProvider(tp))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// The command will fail to connect; we only care that a span was produced.
	_ = client.Get(ctx, "some-key").Err()

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans, "expected redisotel to create a command span")
}

// TestInstrument_NeverEmitsCommandOrKeyAsAttribute is the PII/cardinality
// guardrail: redisotel attaches db.statement (the raw command incl. key/values)
// by default. The helper MUST disable it, so no span carries the command text,
// key, or value as an attribute (docs/metrics-contract.md FORBIDDEN list).
func TestInstrument_NeverEmitsCommandOrKeyAsAttribute(t *testing.T) {
	_, tp, spanExp := newRedisHarness(t)

	client := newUnreachableClient(t)

	require.NoError(t, Instrument(client, WithTracerProvider(tp)))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	const secretKey = "pix:key:123.456.789-00"
	const secretVal = "super-secret-token"

	_ = client.Set(ctx, secretKey, secretVal, 0).Err()
	_ = client.Get(ctx, secretKey).Err()

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)

	for _, s := range spans {
		for _, kv := range s.Attributes {
			key := string(kv.Key)
			val := kv.Value.Emit()

			assert.NotEqual(t, "db.statement", key,
				"db.statement present; WithDBStatement(false) guardrail failed")
			assert.NotEqual(t, "db.query.text", key, "db.query.text present on redis span")

			assert.NotContains(t, val, secretKey, "span attribute leaked redis key: %s=%s", key, val)
			assert.NotContains(t, val, secretVal, "span attribute leaked redis value: %s=%s", key, val)
			assert.NotContains(t, val, "123.456.789-00", "span attribute leaked PII: %s=%s", key, val)
		}
	}
}

// TestInstrument_NilClientReturnsError verifies the helper is nil-safe.
func TestInstrument_NilClientReturnsError(t *testing.T) {
	err := Instrument(nil)
	require.Error(t, err)
}

// TestInstrument_NoTelemetryDoesNotError verifies that with no providers the
// helper still succeeds (degrading to no-op providers) and never breaks the app.
func TestInstrument_NoTelemetryDoesNotError(t *testing.T) {
	client := newUnreachableClient(t)
	require.NoError(t, Instrument(client))
}

// newCollectingHarness builds a MeterProvider backed by a ManualReader so the
// test can see exactly which instruments the meter is collecting at any point,
// which is how the asynchronous pool-stat registrations become observable.
func newCollectingHarness(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	return mp, reader
}

// metricPresent reports whether the reader currently collects a metric with the
// given name. It deliberately takes no *testing.T: it is polled from the
// assert.Eventually goroutine, where a failed assertion would be illegal.
func metricPresent(reader *sdkmetric.ManualReader, name string) bool {
	rm := &metricdata.ResourceMetrics{}
	if err := reader.Collect(context.Background(), rm); err != nil {
		return false
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}

	return false
}

// TestSetup_CleanupReleasesPoolStatRegistrations is the reason Setup exists:
// InstrumentMetrics registers asynchronous callbacks that observe the client's
// pool, and nothing in Instrument can cancel them. A caller that recreates its
// client (a reconnect) would otherwise leak callbacks observing a dead client
// forever. Cleanup MUST make the pool-stat series stop being collected.
func TestSetup_CleanupReleasesPoolStatRegistrations(t *testing.T) {
	mp, reader := newCollectingHarness(t)

	client := newUnreachableClient(t)

	cleanup, err := Setup(client, WithMeterProvider(mp))
	require.NoError(t, err)
	require.NotNil(t, cleanup, "cleanup must never be nil")

	require.True(t, metricPresent(reader, poolStatMetric),
		"expected redisotel to register the pool-stat instruments")

	require.NoError(t, cleanup())

	// redisotel unregisters from the goroutine watching the close channel, so
	// the release is observable shortly after cleanup returns, not instantly.
	assert.Eventually(t, func() bool {
		return !metricPresent(reader, poolStatMetric)
	}, time.Second, 10*time.Millisecond,
		"pool-stat instruments must stop being collected after cleanup")
}

// TestSetup_CleanupIsIdempotent verifies a shutdown path racing a client swap
// can call cleanup twice without panicking on a double close.
func TestSetup_CleanupIsIdempotent(t *testing.T) {
	mp, _ := newCollectingHarness(t)

	cleanup, err := Setup(newUnreachableClient(t), WithMeterProvider(mp))
	require.NoError(t, err)

	require.NoError(t, cleanup())
	assert.NoError(t, cleanup(), "cleanup must be idempotent")
}

// TestSetup_NilClientReturnsError verifies the nil-safety contract: ErrNilClient,
// a usable no-op cleanup, and no panic.
func TestSetup_NilClientReturnsError(t *testing.T) {
	cleanup, err := Setup(nil)

	require.ErrorIs(t, err, ErrNilClient)
	require.NotNil(t, cleanup, "cleanup must never be nil, even on error")
	assert.NoError(t, cleanup())
}

// TestSetup_NoTelemetryDoesNotError verifies no-op degradation (ADR-008): with
// no providers configured Setup still succeeds and still hands back a callable
// cleanup, so telemetry being off never breaks the caller's client.
func TestSetup_NoTelemetryDoesNotError(t *testing.T) {
	cleanup, err := Setup(newUnreachableClient(t))
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	assert.NoError(t, cleanup())
}

// unsupportedClient satisfies redis.UniversalClient without being one of the
// concrete types redisotel can instrument (*redis.Client, *redis.ClusterClient,
// *redis.Ring), which is how the failure path is reached without a live server.
type unsupportedClient struct {
	redis.UniversalClient
}

// TestSetup_UnsupportedClientStillReturnsUsableCleanup is the ADR-008
// degradation rule on the failure path: instrumentation that cannot be applied
// is informational — the caller still gets a non-nil, callable cleanup and
// never has to nil-check what it got back.
func TestSetup_UnsupportedClientStillReturnsUsableCleanup(t *testing.T) {
	cleanup, err := Setup(unsupportedClient{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
	require.NotNil(t, cleanup, "cleanup must never be nil, even on error")
	assert.NoError(t, cleanup())
}

// TestSetup_NeverEmitsCommandOrKeyAsAttribute repeats the PII/cardinality
// guardrail on the new entry point: Setup must disable db.statement exactly like
// Instrument does (docs/metrics-contract.md FORBIDDEN list).
func TestSetup_NeverEmitsCommandOrKeyAsAttribute(t *testing.T) {
	_, tp, spanExp := newRedisHarness(t)

	client := newUnreachableClient(t)

	cleanup, err := Setup(client, WithTracerProvider(tp))
	require.NoError(t, err)

	t.Cleanup(func() { _ = cleanup() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	const secretKey = "pix:key:123.456.789-00"

	_ = client.Get(ctx, secretKey).Err()

	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)

	for _, s := range spans {
		for _, kv := range s.Attributes {
			key := string(kv.Key)
			val := kv.Value.Emit()

			assert.NotEqual(t, "db.statement", key,
				"db.statement present; WithDBStatement(false) guardrail failed")
			assert.NotContains(t, val, secretKey, "span attribute leaked redis key: %s=%s", key, val)
		}
	}
}

// failingMeterProvider hands out a Meter whose observable-gauge constructor
// always fails, which is what makes redisotel.InstrumentMetrics fail for a
// reason OTHER than an unsupported client type — the asymmetry the ordering in
// instrument() depends on.
type failingMeterProvider struct{ embedded.MeterProvider }

func (failingMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return failingMeter{}
}

type failingMeter struct{ noopmetric.Meter }

func (failingMeter) Int64ObservableUpDownCounter(
	string, ...metric.Int64ObservableUpDownCounterOption,
) (metric.Int64ObservableUpDownCounter, error) {
	return nil, errors.New("instrument creation failed")
}

// TestSetup_MetricsFailureLeavesNoTracingHook is the regression guard for the
// order of the two redisotel calls.
//
// go-redis hooks are additive, so any side installed before a failure is
// installed AGAIN when the caller retries. Metrics is the only one of the two
// that can fail for something other than an unsupported client type, so it must
// run first: a metrics failure must leave the client untouched, and the retry
// must produce exactly one span per command rather than two.
//
// Reversing the calls in instrument() makes this test fail on the retry count.
func TestSetup_MetricsFailureLeavesNoTracingHook(t *testing.T) {
	_, tp, spanExp := newRedisHarness(t)

	client := newUnreachableClient(t)

	// First attempt: metrics cannot build its instruments, so Setup fails.
	cleanup, err := Setup(client,
		WithMeterProvider(failingMeterProvider{}),
		WithTracerProvider(tp),
	)
	require.Error(t, err, "a meter that cannot create instruments must fail Setup")
	require.NotNil(t, cleanup)

	t.Cleanup(func() { _ = cleanup() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_ = client.Get(ctx, "some-key").Err()

	require.Empty(t, spanExp.GetSpans(),
		"a failed Setup must leave no tracing hook behind, or the retry duplicates it")

	// Second attempt with a working meter: the caller retries the same client.
	mp := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	retryCleanup, err := Setup(client, WithMeterProvider(mp), WithTracerProvider(tp))
	require.NoError(t, err)

	t.Cleanup(func() { _ = retryCleanup() })

	spanExp.Reset()

	_ = client.Get(ctx, "another-key").Err()

	// redisotel installs a dial hook as well as a process hook, so a command
	// against an unreachable address emits both a "redis.dial" and a command
	// span. Count the command span only: a leftover hook from the failed attempt
	// shows up as a SECOND one of these.
	var commandSpans int

	for _, span := range spanExp.GetSpans() {
		if span.Name == "get" {
			commandSpans++
		}
	}

	assert.Equal(t, 1, commandSpans,
		"exactly one command span; two means the failed attempt left a tracing hook installed")
}
