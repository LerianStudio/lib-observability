//go:build unit

package sqlobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// connectionOpenMetric is one of the pool gauges otelsql registers under the
// db.sql.connection.* namespace. Asserted by name so the test fails loudly if
// the upstream namespace ever drifts.
const connectionOpenMetric = "db.sql.connection.open"

// fakeDSN is the DSN openFake dials. The fake driver ignores it; what matters to
// these tests is that it is NOT empty, so Setup is allowed to swap the handle.
const fakeDSN = "fake://db"

// dsnRequiredDriver stands in for the production drivers (pgx, go-sql-driver)
// that resolve the server address from the DSN: opening with an empty one fails.
// It exists to prove WHY Setup refuses to swap a handle it has no DSN for.
type dsnRequiredDriver struct{}

func (dsnRequiredDriver) Open(dsn string) (driver.Conn, error) {
	if dsn == "" {
		return nil, errors.New("dsnRequiredDriver: no DSN, nothing to dial")
	}

	return &fakeConn{}, nil
}

func init() {
	sql.Register("sqlobs-dsn-required", dsnRequiredDriver{})
}

// collectMetricNames returns every metric name the reader currently exposes.
func collectMetricNames(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var names []string

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}

	return names
}

// TestSetup_InstrumentsAndRegistersPoolStats verifies the single call delivers
// BOTH defaults: the operation duration histogram and the pool gauges.
func TestSetup_InstrumentsAndRegistersPoolStats(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)

	db, cleanup, err := Setup(openFake(t), SystemPostgreSQL,
		WithDSN(fakeDSN),
		WithMeterProvider(mp),
		WithTracerProvider(tp),
		WithPoolRole(PoolRolePrimary),
	)
	require.NoError(t, err)
	require.NotNil(t, db)
	require.NotNil(t, cleanup)

	t.Cleanup(func() {
		_ = cleanup()
		_ = db.Close()
	})

	_, err = db.ExecContext(context.Background(), "INSERT INTO accounts VALUES (1)")
	require.NoError(t, err)

	points := collectDurationDataPoints(t, reader)
	require.NotEmpty(t, points, "expected db.client.operation.duration to be recorded")

	system, ok := attrString(points[0].Attributes, "db.system.name")
	require.True(t, ok, "db.system.name must be present")
	assert.Equal(t, "postgresql", system)

	role, ok := attrString(points[0].Attributes, poolRoleAttrKey)
	require.True(t, ok, "pool role must be present when WithPoolRole is set")
	assert.Equal(t, string(PoolRolePrimary), role)

	assert.Contains(t, collectMetricNames(t, reader), connectionOpenMetric,
		"Setup must register the connection-pool gauges, not just the duration")
}

// TestSetup_ClosesOriginalHandle verifies the ownership contract: the caller's
// handle is closed, so there is no second live pool against the same database.
func TestSetup_ClosesOriginalHandle(t *testing.T) {
	mp, _, tp, _ := newHarness(t)

	raw := openFake(t)

	db, cleanup, err := Setup(raw, SystemPostgreSQL,
		WithDSN(fakeDSN),
		WithMeterProvider(mp),
		WithTracerProvider(tp),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = cleanup()
		_ = db.Close()
	})

	assert.Error(t, raw.PingContext(context.Background()),
		"the uninstrumented handle must be closed by Setup")

	require.NoError(t, db.PingContext(context.Background()),
		"the returned handle must be usable")
}

// TestSetup_WithoutDSNKeepsCallerPoolAlive is the ADR-008 contract at its
// sharpest: instrumentation may degrade, never disconnect. With no DSN there is
// nothing to re-open the driver with, so Setup must refuse the swap outright —
// hand back the caller's own handle, still open, and say why. Both shapes of
// "no DSN" are covered, since an empty string dials exactly as little as an
// absent option.
func TestSetup_WithoutDSNKeepsCallerPoolAlive(t *testing.T) {
	cases := map[string][]Option{
		"WithDSN omitted": nil,
		"WithDSN empty":   {WithDSN("")},
	}

	for name, dsnOpt := range cases {
		t.Run(name, func(t *testing.T) {
			mp, _, tp, _ := newHarness(t)

			raw := openFake(t)

			opts := append([]Option{WithMeterProvider(mp), WithTracerProvider(tp)}, dsnOpt...)

			db, cleanup, err := Setup(raw, SystemPostgreSQL, opts...)

			require.ErrorIs(t, err, ErrDSNRequired)
			require.NotNil(t, cleanup, "cleanup must never be nil, even on error")
			assert.NoError(t, cleanup())

			require.Same(t, raw, db, "Setup must hand back the caller's own handle, not a replacement")

			_, err = db.ExecContext(context.Background(), "SELECT 1")
			require.NoError(t, err, "the caller's pool must still be usable after a refused swap")
		})
	}
}

// TestInstrumentDB_WithoutDSNYieldsPoolThatCannotDial proves the premise behind
// the refusal above: re-opening a DSN-resolving driver with an empty DSN builds a
// pool that dies on its first query. Closing the caller's working handle in
// exchange for this one is the data-loss path Setup must never take.
func TestInstrumentDB_WithoutDSNYieldsPoolThatCannotDial(t *testing.T) {
	raw, err := sql.Open("sqlobs-dsn-required", "host=db port=5432")
	require.NoError(t, err)

	t.Cleanup(func() { _ = raw.Close() })

	require.NoError(t, raw.PingContext(context.Background()),
		"the caller's own handle dials fine — it has the DSN")

	instrumented, err := InstrumentDB(raw, SystemPostgreSQL)
	require.NoError(t, err)
	require.NotNil(t, instrumented)

	t.Cleanup(func() { _ = instrumented.Close() })

	assert.Error(t, instrumented.PingContext(context.Background()),
		"an instrumented pool re-opened with an empty DSN cannot dial")
}

// TestSetup_CleanupUnregistersPoolStats verifies the returned cleanup releases
// the gauges and is safe to call more than once (shutdown racing a pool swap).
func TestSetup_CleanupUnregistersPoolStats(t *testing.T) {
	mp, reader, _, _ := newHarness(t)

	db, cleanup, err := Setup(openFake(t), SystemPostgreSQL, WithDSN(fakeDSN), WithMeterProvider(mp))
	require.NoError(t, err)

	// The handle is retained purely so the fresh pool Setup opened is closed once
	// the cleanup assertions below are done with it.
	t.Cleanup(func() { _ = db.Close() })

	require.Contains(t, collectMetricNames(t, reader), connectionOpenMetric)

	require.NoError(t, cleanup())
	assert.NotContains(t, collectMetricNames(t, reader), connectionOpenMetric,
		"pool gauges must stop being collected after cleanup")

	assert.NoError(t, cleanup(), "cleanup must be idempotent")
}

// TestSetup_NilDB verifies the nil-safety contract: an error, a usable no-op
// cleanup, and no panic.
func TestSetup_NilDB(t *testing.T) {
	db, cleanup, err := Setup(nil, SystemPostgreSQL)

	require.ErrorIs(t, err, ErrNilDB)
	assert.Nil(t, db)
	require.NotNil(t, cleanup, "cleanup must never be nil, even on error")
	assert.NoError(t, cleanup())
}

// TestSetup_NoProvidersStillReturnsWorkingPool verifies no-op degradation: with
// telemetry off the caller still gets a working handle.
func TestSetup_NoProvidersStillReturnsWorkingPool(t *testing.T) {
	db, cleanup, err := Setup(openFake(t), SystemPostgreSQL, WithDSN(fakeDSN))
	require.NoError(t, err)
	require.NotNil(t, db)

	t.Cleanup(func() {
		_ = cleanup()
		_ = db.Close()
	})

	_, err = db.ExecContext(context.Background(), "SELECT 1")
	assert.NoError(t, err)
}

// TestSetupOpen_InstrumentsAndRegistersPoolStats verifies the DSN-first variant
// delivers the same two defaults without a throwaway handle.
func TestSetupOpen_InstrumentsAndRegistersPoolStats(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)

	db, cleanup, err := SetupOpen("sqlobs-fake", "fake://db", SystemMySQL,
		WithMeterProvider(mp),
		WithTracerProvider(tp),
	)
	require.NoError(t, err)
	require.NotNil(t, db)

	t.Cleanup(func() {
		_ = cleanup()
		_ = db.Close()
	})

	_, err = db.ExecContext(context.Background(), "SELECT 1")
	require.NoError(t, err)

	points := collectDurationDataPoints(t, reader)
	require.NotEmpty(t, points)

	system, ok := attrString(points[0].Attributes, "db.system.name")
	require.True(t, ok)
	assert.Equal(t, "mysql", system)

	assert.Contains(t, collectMetricNames(t, reader), connectionOpenMetric)
}
