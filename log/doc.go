// Package log provides a minimal, implementation-agnostic Logger interface with typed
// Field constructors, severity levels, and production-safe error sanitization.
//
// Implementations include GoLogger (stdlib-based with CWE-117 log-injection prevention)
// and NopLogger (no-op for tests and disabled logging).
package log
