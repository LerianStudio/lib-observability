// Package log provides a minimal, implementation-agnostic Logger interface with typed
// Field constructors, severity levels, and production-safe error sanitization.
//
// Implementations include GoLogger (stdlib-based with CWE-117 log-injection prevention)
// and NopLogger (no-op for tests and disabled logging).
//
// Adapt lifts a Log-only logger into a Logger and recovers panics raised inside
// it; a full Logger comes back unchanged. Call Guard(l) where a logger
// you did not write runs on a path that must not panic, such as a library
// boundary that promises to return errors: Guard extends the same recovery to
// every method of any Logger.
package log
