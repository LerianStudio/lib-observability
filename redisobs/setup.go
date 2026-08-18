package redisobs

import (
	"sync"

	"github.com/redis/go-redis/v9"
)

// CleanupFunc releases the resources Setup registered against the meter — today
// the asynchronous pool-stat callbacks (db.client.connections.*). It is always
// non-nil (a no-op when nothing was registered) and safe to call more than once,
// so a shutdown path racing a client swap never double-releases.
type CleanupFunc func() error

// noopCleanup is the CleanupFunc handed back when there is nothing to release.
func noopCleanup() error { return nil }

// onceCleanup makes a release call idempotent, returning the first result on
// every subsequent call.
func onceCleanup(release func() error) CleanupFunc {
	var (
		once sync.Once
		err  error
	)

	return func() error {
		once.Do(func() { err = release() })

		return err
	}
}

// Setup instruments a go-redis UniversalClient exactly like Instrument does and
// additionally returns the cleanup that releases the ASYNCHRONOUS pool-stat
// registrations. Prefer it over Instrument in any long-lived service.
//
// WHY IT EXISTS: redisotel.InstrumentMetrics registers observable instruments
// (db.client.connections.*) whose callbacks hold the client and are owned by the
// MeterProvider, not by the client. They therefore outlive the client: a caller
// that recreates its client — a reconnect, a failover, a per-tenant pool being
// rebuilt — accumulates callbacks observing dead clients, forever, with no way
// to cancel them. Setup wires redisotel's close-channel mechanism so the caller
// finally holds that cancel.
//
// OWNERSHIP (ADR-007) — unchanged: this package still does NOT own the client.
// Setup never dials, never closes, and never manages the client; the returned
// cleanup releases telemetry registrations ONLY. Closing the client stays the
// caller's job, and the two are independent:
//
//	cleanup, err := redisobs.Setup(client, redisobs.WithMeterProvider(mp))
//	if err != nil {
//	    logger.Warnf("redis telemetry degraded: %v", err)
//	}
//	defer cleanup()
//	defer client.Close()
//
// The release is asynchronous: cleanup closes the channel redisotel watches, so
// the unregistration completes shortly after cleanup returns rather than
// synchronously inside it. Nothing observes the client in the meantime beyond
// the next collection cycle at most.
//
// The PII/cardinality guardrail is identical to Instrument's: db.statement (the
// raw command, key, and values) is disabled on spans, unconditionally.
//
// DEGRADATION (ADR-008): telemetry failing must never cost the caller a working
// client. On any failure the returned CleanupFunc is still non-nil and callable
// and the client is left usable — the error is informational and the caller may
// log it and proceed. A nil client returns ErrNilClient and never panics. With
// no providers configured instrumentation attaches against the OTel no-op
// providers and Setup still succeeds.
func Setup(client redis.UniversalClient, opts ...Option) (CleanupFunc, error) {
	if client == nil {
		return noopCleanup, ErrNilClient
	}

	closeChan := make(chan struct{})

	if err := instrument(client, closeChan, opts...); err != nil {
		// InstrumentMetrics starts the watcher goroutine BEFORE it can fail, and
		// a cluster/ring client may already have registered a node by then.
		// Close now so neither the goroutine nor a partial registration outlives
		// the failed call.
		close(closeChan)

		return noopCleanup, err
	}

	return onceCleanup(func() error {
		close(closeChan)

		return nil
	}), nil
}
