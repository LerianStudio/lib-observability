// Package sqlobs provides thin, nil-safe helpers that add OpenTelemetry
// instrumentation to a database/sql handle the application already owns. It
// covers PostgreSQL (via pgx/stdlib or any database/sql driver) and
// MySQL/MariaDB — the two share the database/sql layer, so a single helper
// instruments both, differing only in the parameterizable db.system.name value.
//
// # Boundary (ADR-002, ADR-007)
//
// This package does NOT own connections: it never calls sql.Open on the
// caller's behalf as part of ownership, never builds a dbresolver, and never
// manages a pool's lifecycle. It wraps a *sql.DB (or a driver.Connector) the
// caller created and returns an instrumented handle. For a read/write split
// built on github.com/bxcodec/dbresolver, the caller MUST instrument EACH
// underlying *sql.DB (primary and every replica) with InstrumentDB BEFORE
// passing them to dbresolver.New — the resolver itself is not wrappable because
// it exposes no driver.Driver.
//
// # Emitted telemetry
//
// The wrapped handle emits the OpenTelemetry semantic-convention metric
// db.client.operation.duration (Float64 histogram, unit "s") via XSAM/otelsql,
// carrying db.system.name (postgresql | mysql), db.operation.name, and — when
// the driver/DSN supply them — db.collection.name / db.namespace, plus error.type
// on failures. An optional low-cardinality pool-role attribute (primary |
// replica) can be added with WithPoolRole (ADR-002).
//
// # PII / cardinality guardrail (docs/metrics-contract.md)
//
// otelsql captures db.query.text on spans by default. This package disables that
// unconditionally (SpanOptions.DisableQuery) and never enables SQLCommenter, so
// no query text, SQL statement, or bind parameter is ever emitted as a span or
// metric attribute. This is enforced by tests.
//
// # No-op degradation (ADR-008)
//
// With no MeterProvider/TracerProvider supplied the helper still returns a
// working *sql.DB; instrumentation degrades to the OTel no-op providers. The
// helper never panics and never breaks the caller's connection.
package sqlobs
