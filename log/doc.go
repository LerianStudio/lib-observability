// Package log provides a minimal, implementation-agnostic Logger interface with typed
// Field constructors, severity levels, and production-safe error sanitization.
//
// Implementations include GoLogger (stdlib-based with CWE-117 log-injection prevention)
// and NopLogger (no-op for tests and disabled logging).
//
// For consumers in OTHER modules, the package also exposes Contract and
// the Contract adapter: a mirror of Logger declared with universal types only
// (int, string, any, context.Context), so a downstream module can declare an
// equivalent interface in its own package and stop inheriting this module's
// major version. See universal.go for the full rationale.
package log
