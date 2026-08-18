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
