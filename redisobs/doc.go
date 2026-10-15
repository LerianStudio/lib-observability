// Package redisobs provides a thin, nil-safe helper that adds OpenTelemetry
// tracing and metrics to a go-redis client the application already created. It
// covers both Redis AND Valkey: Valkey is wire-compatible with Redis and uses
// the same github.com/redis/go-redis/v9 driver, so redisotel instruments both
// unchanged (ADR-004) — the emitted db.system value is "redis" in both cases.
//
// # Boundary (ADR-007)
//
// This package does NOT create or own the client. The application builds its
// redis.UniversalClient (single-node, cluster, or failover) and passes it here;
// the helper applies redisotel.InstrumentTracing + redisotel.InstrumentMetrics
// and returns. It never dials, never closes, and never manages the client.
//
// # Releasing the pool-stat registrations
//
// The pool-stat instruments (db.client.connections.*) are ASYNCHRONOUS: their
// callbacks are owned by the MeterProvider, hold the client, and outlive it.
// Instrument returns nothing that can cancel them, so a service that recreates
// its client (reconnect, failover, per-tenant pool rebuild) would accumulate
// callbacks observing dead clients. Setup is Instrument plus the CleanupFunc
// that releases them; prefer it in any long-lived service.
//
// # Emitted telemetry
//
// redisotel emits db.client.operation.duration (seconds) and command spans with
// db.system=redis. No connection is created by this package.
//
// # Spans need a parent span
//
// A command is traced only when its context already holds a valid parent span:
// one started by the request middleware or by the caller, or a remote parent
// extracted from incoming headers. A command with no parent — a background loop,
// a poller, a health check — records no span. On its own it would become a
// separate trace several times per second, even when there is no work, and the
// duration metric already covers it. Metrics are recorded either way.
//
// To trace a background job, start a span around each unit of work and pass
// its context to the commands. Start it only once there is work to do: a span
// per empty poll brings back the traces this rule removes.
//
//	ctx, span := tracer.Start(ctx, "worker.process_batch")
//	defer span.End()
//
//	err := client.Get(ctx, key).Err()
//
// The rule lives in the TracerProvider this package hands to redisotel, not in a
// sampler, so OTEL_TRACES_SAMPLER and spans from other instrumentation are
// unaffected.
//
// # PII / cardinality guardrail (docs/metrics-contract.md)
//
// redisotel attaches db.statement — the raw command including the key and, for
// writes, argument values — to spans by default. This package disables that
// unconditionally (WithDBStatement(false)), so no redis key, value, or command
// text is ever emitted as a span or metric attribute. Enforced by tests.
//
// # No-op degradation (ADR-008)
//
// With no providers supplied, instrumentation attaches against the OTel no-op
// providers; the helper never panics and never breaks the client.
package redisobs
