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
// Spans are kept to the operations that show where DB time went: sql.conn.query,
// sql.conn.exec, sql.stmt.exec / sql.stmt.query, sql.conn.begin_tx,
// sql.tx.commit and sql.tx.rollback. The sql.rows, sql.conn.reset_session,
// sql.conn.prepare, sql.connector.connect and sql.conn.ping spans are never
// emitted: they usually last microseconds and multiply the span count of every
// trace. The duration metric is still recorded for every operation, including
// the ones without a span. WithSpanFilter drops more spans, for example
// begin/commit; it cannot bring back an omitted span or the query text.
//
// # Spans need a parent span
//
// A query is traced only when its context already holds a valid parent span:
// one started by the request middleware or by the caller, or a remote parent
// extracted from incoming headers. A query with no parent — a background loop,
// a poller, a health check — records no span. On its own it would become a
// separate trace several times per second, even when there is no work, and the
// duration metric already covers it. Metrics are recorded either way.
//
// To trace a background job, start a span around each unit of work and pass
// its context to the queries. Start it only once there is work to do: a span
// per empty poll brings back the traces this rule removes.
//
//	ctx, span := tracer.Start(ctx, "worker.process_batch")
//	defer span.End()
//
//	rows, err := db.QueryContext(ctx, query)
//
// The rule lives in the TracerProvider this package hands to otelsql, not in a
// sampler, so OTEL_TRACES_SAMPLER and spans from other instrumentation are
// unaffected.
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
