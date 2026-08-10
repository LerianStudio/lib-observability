//go:build unit

package redisobs

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

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
