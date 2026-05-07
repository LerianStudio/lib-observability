// Package runtime provides policy-driven panic recovery with full observability
// integration: span event recording, panic counter metrics, structured logging,
// and optional external error reporter forwarding. Includes safe goroutine launchers
// and production mode for stack trace redaction.
package runtime
