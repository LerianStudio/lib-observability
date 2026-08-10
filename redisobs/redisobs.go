package redisobs

import (
	"errors"

	constant "github.com/LerianStudio/lib-observability/v3/constants"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrNilClient is returned by Instrument when the supplied client is nil.
var ErrNilClient = errors.New("redisobs: nil redis.UniversalClient")

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
func Instrument(client redis.UniversalClient, opts ...Option) error {
	if client == nil {
		return ErrNilClient
	}

	cfg := newConfig(opts...)

	// Common attributes shared by tracing and metrics. db.system=redis is the
	// value redisotel already uses; the shared list carries only bounded extras.
	commonAttrs := make([]attribute.KeyValue, 0, 1+len(cfg.extraAttrs))
	commonAttrs = append(commonAttrs, cfg.extraAttrs...)

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

	if err := redisotel.InstrumentTracing(client, tracingOpts...); err != nil {
		return err
	}

	metricsOpts := []redisotel.MetricsOption{
		redisotel.WithMeterProvider(cfg.meterProvider),
	}
	if len(commonAttrs) > 0 {
		metricsOpts = append(metricsOpts, redisotel.WithAttributes(commonAttrs...))
	}

	return redisotel.InstrumentMetrics(client, metricsOpts...)
}

// System returns the db.system value redisotel emits for both Redis and Valkey.
// Exposed so callers building dashboards/tests can reference the canonical value
// without hardcoding it.
func System() string {
	return constant.DBSystemRedis
}
