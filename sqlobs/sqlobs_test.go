//go:build unit

package sqlobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// dbClientOperationDurationMetric is the semconv metric name emitted by otelsql
// for database client operation duration. Asserted here so the test fails loudly
// if the underlying instrument name ever drifts from the contract.
const dbClientOperationDurationMetric = "db.client.operation.duration"

// --- Minimal in-memory database/sql driver for the harness --------------------
//
// The lib does NOT own connections; these fakes exist only so the test can drive
// a real *sql.DB through otelsql and observe the emitted telemetry. They record
// nothing about the query text, mirroring how a production driver would be
// wrapped.

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (*fakeConn) Prepare(query string) (driver.Stmt, error) { return &fakeStmt{query: query}, nil }
func (*fakeConn) Close() error                              { return nil }
func (*fakeConn) Begin() (driver.Tx, error)                 { return &fakeTx{}, nil }

// QueryContext lets the driver satisfy driver.QueryerContext so ExecContext /
// QueryContext flow straight through without the prepared-statement fallback.
func (*fakeConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return &fakeRows{}, nil
}

func (*fakeConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (*fakeConn) Ping(context.Context) error { return nil }

type fakeStmt struct{ query string }

func (*fakeStmt) Close() error                               { return nil }
func (*fakeStmt) NumInput() int                              { return 0 }
func (*fakeStmt) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(0), nil }
func (*fakeStmt) Query([]driver.Value) (driver.Rows, error)  { return &fakeRows{}, nil }

type fakeRows struct{}

func (*fakeRows) Columns() []string         { return []string{} }
func (*fakeRows) Close() error              { return nil }
func (*fakeRows) Next([]driver.Value) error { return io.EOF }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

func init() {
	sql.Register("sqlobs-fake", fakeDriver{})
}

// --- Harness ------------------------------------------------------------------

func newHarness(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader, *sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	return mp, reader, tp, spanExp
}

// openFake opens a raw *sql.DB against the in-test fake driver, standing in for
// the connection the app already created before handing it to the helper.
func openFake(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlobs-fake", "fake://db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func collectDurationDataPoints(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var points []metricdata.HistogramDataPoint[float64]

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != dbClientOperationDurationMetric {
				continue
			}

			require.Equal(t, "s", m.Unit, "db duration must be seconds")

			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected float64 histogram, got %T", m.Data)
			points = append(points, h.DataPoints...)
		}
	}

	return points
}

func attrString(set attribute.Set, key string) (string, bool) {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return "", false
	}

	return v.AsString(), true
}

// TestInstrumentDB_EmitsDurationForPostgres verifies a real query through the
// wrapped *sql.DB emits db.client.operation.duration in seconds with the
// parameterizable db.system.name and a db.operation.name attribute.
func TestInstrumentDB_EmitsDurationForPostgres(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)

	raw := openFake(t)

	db, err := InstrumentDB(raw, SystemPostgreSQL,
		WithMeterProvider(mp),
		WithTracerProvider(tp),
	)
	require.NoError(t, err)
	require.NotNil(t, db)

	_, err = db.ExecContext(context.Background(), "INSERT INTO accounts VALUES (1)")
	require.NoError(t, err)

	points := collectDurationDataPoints(t, reader)
	require.NotEmpty(t, points, "expected db.client.operation.duration to be recorded")

	var found bool

	for _, dp := range points {
		system, ok := attrString(dp.Attributes, "db.system.name")
		if !ok {
			continue
		}

		assert.Equal(t, "postgresql", system)

		_, hasOp := attrString(dp.Attributes, "db.operation.name")
		assert.True(t, hasOp, "db.operation.name must be present")

		found = true
	}

	assert.True(t, found, "at least one data point must carry db.system.name=postgresql")
}

// TestInstrumentDB_SystemNameParameterizable verifies mysql is emitted when the
// caller selects the MySQL system, proving the same helper covers MySQL/MariaDB.
func TestInstrumentDB_SystemNameParameterizable(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)

	db, err := InstrumentDB(openFake(t), SystemMySQL,
		WithMeterProvider(mp),
		WithTracerProvider(tp),
	)
	require.NoError(t, err)

	_, err = db.ExecContext(context.Background(), "SELECT 1")
	require.NoError(t, err)

	points := collectDurationDataPoints(t, reader)
	require.NotEmpty(t, points)

	var systems []string
	for _, dp := range points {
		if s, ok := attrString(dp.Attributes, "db.system.name"); ok {
			systems = append(systems, s)
		}
	}

	assert.Contains(t, systems, "mysql")
}

// TestInstrumentDB_NeverEmitsQueryText is the PII/cardinality guardrail: no
// metric data point and no span may carry query text, SQL statement, or bind
// parameters as an attribute (docs/metrics-contract.md FORBIDDEN list).
func TestInstrumentDB_NeverEmitsQueryText(t *testing.T) {
	mp, reader, tp, spanExp := newHarness(t)

	db, err := InstrumentDB(openFake(t), SystemPostgreSQL,
		WithMeterProvider(mp),
		WithTracerProvider(tp),
	)
	require.NoError(t, err)

	const secret = "SELECT secret_column FROM pix_keys WHERE cpf = '123.456.789-00'"

	rows, err := db.QueryContext(context.Background(), secret)
	require.NoError(t, err)
	require.NoError(t, rows.Close())

	forbidden := []string{
		"db.statement",
		"db.query.text",
		"db.query.parameter",
		"sql",
		"statement",
	}

	// Metric side.
	points := collectDurationDataPoints(t, reader)
	require.NotEmpty(t, points)

	for _, dp := range points {
		for _, kv := range dp.Attributes.ToSlice() {
			key := string(kv.Key)
			val := kv.Value.Emit()

			for _, bad := range forbidden {
				assert.NotEqual(t, bad, key, "forbidden label %q present on db duration metric", bad)
			}

			assert.NotContains(t, val, "secret_column", "metric label leaked query text: %s=%s", key, val)
			assert.NotContains(t, val, "cpf", "metric label leaked PII column: %s=%s", key, val)
		}
	}

	// Span side (otelsql captures db.query.text on spans by default; the helper
	// MUST disable that).
	for _, s := range spanExp.GetSpans() {
		for _, kv := range s.Attributes {
			key := string(kv.Key)
			val := kv.Value.Emit()

			assert.NotEqual(t, "db.query.text", key, "span carries db.query.text; DisableQuery guardrail failed")
			assert.NotEqual(t, "db.statement", key, "span carries db.statement")
			assert.NotContains(t, val, "secret_column", "span attribute leaked query text: %s=%s", key, val)
		}
	}
}

// TestInstrumentDB_PoolRoleAttribute verifies the optional primary/replica
// distinguishing attribute (ADR-002) is emitted on the metric when supplied,
// bounded to a low-cardinality value.
func TestInstrumentDB_PoolRoleAttribute(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)

	db, err := InstrumentDB(openFake(t), SystemPostgreSQL,
		WithMeterProvider(mp),
		WithTracerProvider(tp),
		WithPoolRole(PoolRoleReplica),
	)
	require.NoError(t, err)

	_, err = db.ExecContext(context.Background(), "SELECT 1")
	require.NoError(t, err)

	points := collectDurationDataPoints(t, reader)
	require.NotEmpty(t, points)

	var found bool
	for _, dp := range points {
		if role, ok := attrString(dp.Attributes, poolRoleAttrKey); ok {
			assert.Equal(t, "replica", role)
			found = true
		}
	}

	assert.True(t, found, "pool role attribute must be present when WithPoolRole is set")
}

// TestInstrumentDB_NilDBReturnsError verifies the helper is nil-safe: a nil
// input never panics and never returns a usable-but-broken handle.
func TestInstrumentDB_NilDBReturnsError(t *testing.T) {
	db, err := InstrumentDB(nil, SystemPostgreSQL)
	require.Error(t, err)
	assert.Nil(t, db)
}

// TestInstrumentDB_NoTelemetryReturnsOriginal verifies that with no providers
// configured the helper degrades to a no-op passthrough, returning a working
// *sql.DB (the original) so the app is never broken by telemetry being off.
func TestInstrumentDB_NoTelemetryReturnsOriginal(t *testing.T) {
	raw := openFake(t)

	db, err := InstrumentDB(raw, SystemPostgreSQL)
	require.NoError(t, err)
	require.NotNil(t, db)

	// The returned handle must still be usable.
	_, err = db.ExecContext(context.Background(), "SELECT 1")
	require.NoError(t, err)
}

// TestRegisterDBStatsMetrics_EmitsPoolMetrics verifies the opt-in pool metrics
// helper registers low-cardinality connection-pool gauges.
func TestRegisterDBStatsMetrics_EmitsPoolMetrics(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)

	db, err := InstrumentDB(openFake(t), SystemPostgreSQL,
		WithMeterProvider(mp),
		WithTracerProvider(tp),
	)
	require.NoError(t, err)

	reg, err := RegisterDBStatsMetrics(db, SystemPostgreSQL, WithMeterProvider(mp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Unregister() })

	// Force at least one connection so the pool has observable state.
	_, err = db.ExecContext(context.Background(), "SELECT 1")
	require.NoError(t, err)

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}

	var hasPoolMetric bool
	for _, n := range names {
		if n == "db.sql.connection.open" || n == "db.sql.connection.max_open" {
			hasPoolMetric = true
		}
	}

	assert.True(t, hasPoolMetric, "expected db.sql.connection.* pool metrics, got: %v", names)
}
