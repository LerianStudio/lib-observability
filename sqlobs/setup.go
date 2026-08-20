package sqlobs

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// CleanupFunc releases the resources Setup registered against the meter — today
// the connection-pool gauges. It is always non-nil (a no-op when nothing was
// registered) and safe to call more than once, so a shutdown path racing a pool
// swap never double-unregisters.
type CleanupFunc func() error

// noopCleanup is the CleanupFunc handed back when there is nothing to release.
func noopCleanup() error { return nil }

// onceCleanup makes an unregister call idempotent, returning the first result on
// every subsequent call.
func onceCleanup(unregister func() error) CleanupFunc {
	var (
		once sync.Once
		err  error
	)

	return func() error {
		once.Do(func() { err = unregister() })

		return err
	}
}

// registerStats registers the pool gauges and adapts the OTel Registration into
// a CleanupFunc. On failure it returns a no-op cleanup alongside the error, so
// the caller never has to nil-check what it got back.
func registerStats(db *sql.DB, system System, opts ...Option) (CleanupFunc, error) {
	reg, err := RegisterDBStatsMetrics(db, system, opts...)
	if err != nil {
		return noopCleanup, fmt.Errorf("sqlobs: registering pool stats metrics: %w", err)
	}

	return onceCleanup(reg.Unregister), nil
}

// Setup applies the full default instrumentation for a SQL pool in ONE call:
// it instruments the handle (db.client.operation.duration) and registers the
// connection-pool gauges (db.sql.connection.*), returning the handle to use from
// now on plus the cleanup that releases those gauges.
//
// It composes InstrumentDB + RegisterDBStatsMetrics so callers do not have to
// know the otelsql ownership rules that make the two-step form easy to get
// wrong. Prefer Setup over calling those two directly.
//
// OWNERSHIP — the returned handle REPLACES db:
//
//   - db is CLOSED by this function. InstrumentDB backs the instrumented handle
//     with a fresh, independent pool, so keeping both alive would mean two pools
//     against the same database. sql.Open is lazy — db has not dialed anything
//     yet — so closing it here releases no live connection.
//   - the caller MUST use only the returned *sql.DB afterwards.
//
// ORDER — call Setup BEFORE applying pool tuning. SetMaxOpenConns /
// SetMaxIdleConns / SetConnMaxLifetime / SetConnMaxIdleTime are per-*sql.DB and
// are NOT carried over from db, so apply them to the returned handle:
//
//	db, cleanup, err := sqlobs.Setup(raw, sqlobs.SystemPostgreSQL,
//	    sqlobs.WithDSN(dsn), sqlobs.WithPoolRole(sqlobs.PoolRolePrimary))
//	db.SetMaxOpenConns(25)
//
// A NON-EMPTY WithDSN IS REQUIRED, and enforced: a *sql.DB does not expose the
// DSN it was opened with, so with none supplied (omitted, or empty) the
// replacement pool would have nothing to dial. Setup therefore refuses the swap —
// it returns db untouched (still open), a no-op cleanup, and ErrDSNRequired.
// Prefer SetupOpen when the DSN is known at construction time — it never creates
// the throwaway pool at all.
//
// For a dbresolver read/write split, call Setup on EACH *sql.DB (primary and
// every replica) BEFORE building the resolver (ADR-002); the resolver itself is
// not wrappable.
//
// DEGRADATION (ADR-008): instrumentation never breaks connectivity. On any
// failure the returned *sql.DB is still a usable handle and the returned
// CleanupFunc is still non-nil; the error is informational and the caller may
// log it and proceed. A nil db returns ErrNilDB and never panics. A missing DSN
// returns ErrDSNRequired with db untouched. With no telemetry providers
// configured the handle is a plain working *sql.DB.
func Setup(db *sql.DB, system System, opts ...Option) (*sql.DB, CleanupFunc, error) {
	if db == nil {
		return nil, noopCleanup, ErrNilDB
	}

	// The swap is only safe when the replacement pool can dial, and InstrumentDB
	// always re-opens the driver by DSN. With an empty DSN the swap would close a
	// working pool and hand back one that fails on its first query — and sql.Open
	// being lazy means it would not even fail here, but later, in the caller's
	// money path. Refuse instead: telemetry is never worth connectivity.
	if newConfig(opts...).dsn == "" {
		return db, noopCleanup, ErrDSNRequired
	}

	instrumented, err := InstrumentDB(db, system, opts...)
	if err != nil {
		// Hand back the caller's own handle untouched: telemetry failing must
		// not cost them a working pool.
		return db, noopCleanup, fmt.Errorf("sqlobs: instrumenting pool: %w", err)
	}

	var errs []error

	if cerr := db.Close(); cerr != nil {
		errs = append(errs, fmt.Errorf("sqlobs: closing uninstrumented handle: %w", cerr))
	}

	cleanup, rerr := registerStats(instrumented, system, opts...)
	if rerr != nil {
		errs = append(errs, rerr)
	}

	return instrumented, cleanup, errors.Join(errs...)
}

// SetupOpen opens an instrumented pool directly from a driver name and DSN and
// registers the connection-pool gauges, applying the same defaults as Setup.
//
// Prefer this over Setup when the DSN is known at construction time: the pool is
// instrumented from birth, so there is no uninstrumented handle to discard and
// no ownership rule to remember. The caller still owns the returned handle's
// lifecycle (Close) and still applies pool tuning to it.
//
// DEGRADATION (ADR-008): a failure to open is returned as an error with a nil
// handle — that is a real connection failure, not a telemetry one. A failure to
// register the pool gauges returns a usable handle, a no-op cleanup, and an
// informational error.
func SetupOpen(driverName, dsn string, system System, opts ...Option) (*sql.DB, CleanupFunc, error) {
	db, err := Open(driverName, dsn, system, opts...)
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("sqlobs: opening instrumented pool: %w", err)
	}

	cleanup, rerr := registerStats(db, system, opts...)

	return db, cleanup, rerr
}
