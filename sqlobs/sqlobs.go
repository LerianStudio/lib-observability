package sqlobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"

	constant "github.com/LerianStudio/lib-observability/v2/constants"
	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// System identifies the database management system for the db.system.name
// attribute. Only the values the platform actually runs are exposed, keeping the
// label bounded.
type System string

const (
	// SystemPostgreSQL is the db.system.name value for PostgreSQL (incl. pgx/stdlib).
	SystemPostgreSQL System = System(constant.DBSystemPostgreSQL)
	// SystemMySQL is the db.system.name value for MySQL and MariaDB (same driver layer).
	SystemMySQL System = "mysql"
)

// PoolRole is the optional low-cardinality attribute distinguishing a primary
// (read/write) pool from a replica (read-only) pool in a read/write split
// (ADR-002). Bounded to two values.
type PoolRole string

const (
	// PoolRolePrimary marks the read/write pool.
	PoolRolePrimary PoolRole = "primary"
	// PoolRoleReplica marks a read-only replica pool.
	PoolRoleReplica PoolRole = "replica"
)

// poolRoleAttrKey is the metric/span attribute key carrying the PoolRole. It is
// namespaced under db.sql to signal it is a library extension, not a semconv
// attribute.
const poolRoleAttrKey = "db.sql.pool.role"

// ErrNilDB is returned by InstrumentDB / RegisterDBStatsMetrics when the caller
// passes a nil *sql.DB.
var ErrNilDB = errors.New("sqlobs: nil *sql.DB")

// config holds resolved helper options.
type config struct {
	meterProvider  metric.MeterProvider
	tracerProvider trace.TracerProvider
	poolRole       PoolRole
	dsn            string
	extraAttrs     []attribute.KeyValue
}

// Option configures the SQL instrumentation helpers.
type Option func(*config)

// WithMeterProvider sets the MeterProvider used for the duration/pool metrics.
// When unset, the global (no-op unless SetMeterProvider was called) provider is
// used, so metrics degrade to no-op rather than breaking the caller.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.meterProvider = mp
		}
	}
}

// WithTracerProvider sets the TracerProvider used for query spans. When unset,
// the global provider is used.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		if tp != nil {
			c.tracerProvider = tp
		}
	}
}

// WithPoolRole adds the optional primary/replica attribute (ADR-002) to every
// emitted metric and span, giving read-vs-write visibility for a dbresolver
// split. Empty values are ignored.
func WithPoolRole(role PoolRole) Option {
	return func(c *config) {
		if role != "" {
			c.poolRole = role
		}
	}
}

// WithDSN supplies the data source name used to re-open the connection through
// the instrumented driver when instrumenting an existing *sql.DB via
// InstrumentDB. A *sql.DB does not expose its DSN, so it must be provided here;
// when omitted, the driver is re-opened with an empty DSN (valid for drivers
// that resolve configuration elsewhere). Prefer Open when the DSN is known at
// construction time.
func WithDSN(dsn string) Option {
	return func(c *config) {
		c.dsn = dsn
	}
}

// WithAttributes appends additional low-cardinality, PII-free attributes to
// every metric and span. Callers are responsible for keeping these bounded;
// query text / parameters / IDs are FORBIDDEN (docs/metrics-contract.md).
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

// baseAttributes builds the attribute slice applied to every span and metric:
// db.system.name plus the optional pool role and caller extras. Query text is
// never included.
func (c config) baseAttributes(system System) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2+len(c.extraAttrs))
	attrs = append(attrs, attribute.String("db.system.name", string(system)))

	if c.poolRole != "" {
		attrs = append(attrs, attribute.String(poolRoleAttrKey, string(c.poolRole)))
	}

	attrs = append(attrs, c.extraAttrs...)

	return attrs
}

// otelsqlOptions translates the resolved config into otelsql options, always
// applying the PII/cardinality guardrail: db.query.text is suppressed on spans
// and SQLCommenter is never enabled, so no query text or bind parameters reach
// spans or metrics.
func (c config) otelsqlOptions(system System) []otelsql.Option {
	return []otelsql.Option{
		otelsql.WithMeterProvider(c.meterProvider),
		otelsql.WithTracerProvider(c.tracerProvider),
		otelsql.WithAttributes(c.baseAttributes(system)...),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			// GUARDRAIL (ADR-002 §3, docs/metrics-contract.md): never attach
			// db.query.text to spans. otelsql captures it by default.
			DisableQuery: true,
			// Ping / RowsNext spans add cardinality/noise with no operational
			// value for a duration signal; leave them off (also the otelsql
			// default, set explicitly for intent).
			Ping:     false,
			RowsNext: false,
		}),
	}
}

// dsnConnector adapts a driver.Driver + DSN into a driver.Connector so an
// instrumented driver can back a fresh *sql.DB. It mirrors the stdlib's
// unexported dsnConnector.
type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c dsnConnector) Driver() driver.Driver { return c.driver }

// InstrumentDB returns a new *sql.DB that wraps the same underlying driver as
// db with OpenTelemetry instrumentation, emitting db.client.operation.duration
// (seconds) tagged with db.system.name=system. The caller keeps ownership of
// the connection lifecycle; this helper only adds instrumentation.
//
// Because a *sql.DB does not expose its DSN, supply it via WithDSN when the
// driver needs it to open connections (most do). When no DSN is given the
// underlying driver is re-opened with an empty DSN.
//
// For a dbresolver read/write split, call InstrumentDB on EACH *sql.DB (primary
// and every replica) BEFORE building the resolver (ADR-002); the resolver is not
// wrappable.
//
// Nil-safe: a nil db returns ErrNilDB and never panics. With no telemetry
// providers configured the returned handle is still a working *sql.DB (no-op
// instrumentation), so telemetry being off never breaks the caller.
func InstrumentDB(db *sql.DB, system System, opts ...Option) (*sql.DB, error) {
	if db == nil {
		return nil, ErrNilDB
	}

	cfg := newConfig(opts...)

	wrappedDriver := otelsql.WrapDriver(db.Driver(), cfg.otelsqlOptions(system)...)

	// Prefer the context-aware connector API when the underlying (and thus the
	// wrapped) driver implements DriverContext, so those features are preserved;
	// otherwise fall back to a plain DSN connector.
	if dc, ok := wrappedDriver.(driver.DriverContext); ok {
		connector, err := dc.OpenConnector(cfg.dsn)
		if err != nil {
			return nil, err
		}

		return sql.OpenDB(connector), nil
	}

	return sql.OpenDB(dsnConnector{dsn: cfg.dsn, driver: wrappedDriver}), nil
}

// Open opens a new instrumented *sql.DB directly from a driver name and DSN,
// applying the same guardrails as InstrumentDB. Prefer this when the DSN is
// known at construction time; it avoids opening an uninstrumented handle first.
// The caller still owns the returned handle's lifecycle.
func Open(driverName, dsn string, system System, opts ...Option) (*sql.DB, error) {
	cfg := newConfig(opts...)

	return otelsql.Open(driverName, dsn, cfg.otelsqlOptions(system)...)
}

// RegisterDBStatsMetrics registers the opt-in, low-cardinality connection-pool
// metrics (db.sql.connection.*: open/idle/max_open/wait/...) for an instrumented
// *sql.DB, tagged with db.system.name=system and any configured pool role. These
// are safe to always enable — the pool exposes a fixed, tiny set of gauges. Call
// Unregister on the returned Registration when the pool is closed.
//
// Nil-safe: a nil db returns ErrNilDB.
func RegisterDBStatsMetrics(db *sql.DB, system System, opts ...Option) (metric.Registration, error) {
	if db == nil {
		return nil, ErrNilDB
	}

	cfg := newConfig(opts...)

	return otelsql.RegisterDBStatsMetrics(db,
		otelsql.WithMeterProvider(cfg.meterProvider),
		otelsql.WithAttributes(cfg.baseAttributes(system)...),
	)
}
