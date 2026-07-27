package tracing

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// StartClientSpan starts a span already classified as an outbound call to an
// external dependency (SpanKind = CLIENT), so callers instrumenting a network
// hop by hand do not have to remember to pass trace.WithSpanKind.
//
// PREFER A WRAPPER WHEN ONE EXISTS. This helper is the LAST resort, for outbound
// calls that have NO dedicated wrapper:
//   - SQL              → use sqlobs
//   - Redis / Valkey   → use redisobs
//   - HTTP client      → use httpobs
//   - messaging        → use messagingobs
//   - inbound (server) → use the middleware / grpcmiddleware
//
// Use StartClientSpan only for the remainder, e.g. the document database (no
// stable driver instrumentation, see ADR-003) or a custom RPC/SDK call. Do NOT
// wrap a call that already has automatic instrumentation AND also open a
// StartClientSpan around it — that double-instruments the same operation.
//
// The CLIENT kind is applied as an overridable default: it is PREPENDED to opts,
// so a caller that deliberately passes its own trace.WithSpanKind(...) wins
// (span start options are last-wins).
//
// This helper only sets the span kind. It does NOT emit metrics and does NOT
// enforce any PII/cardinality contract on name or attributes: the caller owns a
// low-cardinality, PII-free span name and attributes.
func StartClientSpan(
	ctx context.Context,
	tracer trace.Tracer,
	name string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	// Prepend the CLIENT default so an explicit caller kind (appearing later)
	// overrides it (ADR-004).
	withClient := append(
		[]trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindClient)},
		opts...,
	)

	// The span is deliberately returned to the caller to end, exactly like
	// trace.Tracer.Start; spancheck cannot see the caller's End() from here.
	//nolint:spancheck // span ownership is transferred to the caller
	return tracer.Start(ctx, name, withClient...)
}
