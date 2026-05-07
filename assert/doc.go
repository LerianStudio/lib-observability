// Package assert provides context-scoped production assertions that validate domain
// invariants at runtime without panicking. Every assertion failure returns an error,
// records a span event, and increments a metric counter. Includes a predicates library
// for financial domain validation alongside general-purpose checks.
package assert
