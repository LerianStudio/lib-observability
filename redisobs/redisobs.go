package redisobs

import (
	"errors"
	"reflect"

	constant "github.com/LerianStudio/lib-observability/v3/constants"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrNilClient is returned by Instrument and Setup when the supplied client is
// nil, including a typed nil.
var ErrNilClient = errors.New("redisobs: nil redis.UniversalClient")

// isNilClient reports whether client carries no usable client — either an
// untyped nil or a TYPED nil, a non-nil interface holding a nil pointer such as
// an uninitialised *redis.Client field. The distinction matters because a typed
// nil passes `client == nil` and then reaches redisotel, whose type switch
// matches the concrete type and dereferences it: the caller's unset field becomes
// a SIGSEGV inside a dependency instead of ErrNilClient.
func isNilClient(client redis.UniversalClient) bool {
	if client == nil {
		return true
	}

	v := reflect.ValueOf(client)

	return v.Kind() == reflect.Ptr && v.IsNil()
}

// config holds resolved helper options.
type config struct {
	meterProvider  metric.MeterProvider
	tracerProvider trace.TracerProvider
	extraAttrs     []attribute.KeyValue
}

// Option configures the redis instrumentation helper.
type Option func(*config)

// WithMeterProvider sets the MeterProvider for db.client.operation.duration.
// When unset the global provider is used (no-op unless configured).
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.meterProvider = mp
		}
	}
}

// WithTracerProvider sets the TracerProvider for redis command spans. When unset
// the global provider is used.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		if tp != nil {
			c.tracerProvider = tp
		}
	}
}

// WithAttributes appends additional low-cardinality, PII-free attributes to the
// redis spans and metrics. Keys, values, and command text are FORBIDDEN
// (docs/metrics-contract.md) and are never added by this helper.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *config) {
		c.extraAttrs = append(c.extraAttrs, attrs...)
	}
}

func newConfig(opts ...Option) config {
	cfg := config{
		meterProvider:  otel.GetMeterProvider(),
		tracerProvider: otel.GetTracerProvider(),
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// Instrument applies OpenTelemetry tracing and metrics to a go-redis
// UniversalClient (covering Redis and Valkey). The application owns the client;
// this helper only attaches hooks.
//
// The PII/cardinality guardrail is always enforced: db.statement (raw command,
// key, and values) is disabled on spans via WithDBStatement(false).
//
// Nil-safe: a nil client returns ErrNilClient and never panics. With no
// providers configured it attaches against the no-op providers, so telemetry
// being off never breaks the client.
//
// The asynchronous pool-stat instruments (db.client.connections.*) registered
// here live as long as the meter: this call hands back nothing that can release
// them. A caller that ever REPLACES its client — a reconnect, a failover, a
// test that rebuilds the client per case — MUST use Setup instead, which
// returns the cleanup that unregisters them.
func Instrument(client redis.UniversalClient, opts ...Option) error {
	// A nil close channel keeps this call exactly as cheap as it has always
	// been: redisotel then starts no watcher goroutine, because there is no
	// cleanup handle to give the caller anyway.
	return instrument(client, nil, opts...)
}

// instrument applies the redisotel tracing and metrics hooks to client. When
// closeChan is non-nil it is handed to InstrumentMetrics, which unregisters
// every pool-stat registration it made once the channel is closed; a nil
// channel disables that mechanism (and its watcher goroutine) entirely.
//
// Shared by Instrument and Setup so the two entry points can never drift on the
// PII guardrail or on which providers reach redisotel.
func instrument(client redis.UniversalClient, closeChan chan struct{}, opts ...Option) error {
	if isNilClient(client) {
		return ErrNilClient
	}

	cfg := newConfig(opts...)

	// Common attributes shared by tracing and metrics. db.system=redis is the
	// value redisotel already uses; the shared list carries only bounded extras.
	commonAttrs := make([]attribute.KeyValue, 0, 1+len(cfg.extraAttrs))
	commonAttrs = append(commonAttrs, cfg.extraAttrs...)

	metricsOpts := []redisotel.MetricsOption{
		redisotel.WithMeterProvider(cfg.meterProvider),
	}
	if len(commonAttrs) > 0 {
		metricsOpts = append(metricsOpts, redisotel.WithAttributes(commonAttrs...))
	}

	// Metrics-only: the close channel releases the asynchronous pool-stat
	// callbacks. Tracing hooks carry no registration — they are owned by the
	// client and die with it.
	if closeChan != nil {
		metricsOpts = append(metricsOpts, redisotel.WithCloseChan(closeChan))
	}

	// ORDER IS LOAD-BEARING: metrics FIRST, tracing second.
	//
	// go-redis hooks are additive, so a partial failure that leaves one side
	// installed makes a caller's retry attach that side twice. The two calls are
	// not symmetric, which is what settles the order: InstrumentTracing fails for
	// exactly one reason, an unsupported client type, while InstrumentMetrics
	// fails for that AND for instrument-creation errors when it registers the
	// pool-stat callbacks.
	//
	// Running metrics first makes the reachable partial failure impossible: if
	// metrics fails, tracing has not run, so nothing is installed and a retry is
	// clean; if metrics succeeds, the client type is supported and tracing
	// therefore cannot fail. Swapping these back reintroduces a client that
	// double-reports spans after a retry.
	if err := redisotel.InstrumentMetrics(client, metricsOpts...); err != nil {
		return err
	}

	tracingOpts := []redisotel.TracingOption{
		redisotel.WithTracerProvider(cfg.tracerProvider),
		// GUARDRAIL (ADR-004, docs/metrics-contract.md): never attach the raw
		// command / key / value (db.statement) to spans. redisotel enables it by
		// default.
		redisotel.WithDBStatement(false),
	}
	if len(commonAttrs) > 0 {
		tracingOpts = append(tracingOpts, redisotel.WithAttributes(commonAttrs...))
	}

	return redisotel.InstrumentTracing(client, tracingOpts...)
}

// System returns the db.system value redisotel emits for both Redis and Valkey.
// Exposed so callers building dashboards/tests can reference the canonical value
// without hardcoding it.
func System() string {
	return constant.DBSystemRedis
}
