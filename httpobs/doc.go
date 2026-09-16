// Package httpobs provides a thin, nil-safe helper that wraps an outbound HTTP
// transport with OpenTelemetry client instrumentation. Every outbound request is
// classified as a call to an external dependency (span kind CLIENT) and emits
// http.client.request.duration, giving RED visibility of the dependencies a
// service calls (identity provider, BACEN/SPB, tenant-manager, etc.), and the
// matching inbound helper for a stdlib net/http server (NewHandler: span kind
// SERVER, http.server.request.duration). A Fiber v3 app uses middleware instead
// - never both on the same server.
//
// It is a thin wrapper over go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp:
// httpobs applies the library's corporate defaults and does not reimplement span,
// metric, or context-propagation logic.
//
// # Precedence (ADR-006)
//
// PREFER this wrapper for outbound HTTP. For each outbound call type, use the
// dedicated wrapper — SQL: sqlobs, Redis/Valkey: redisobs, HTTP client: httpobs,
// messaging: messagingobs; inbound server: middleware (Fiber), grpcmiddleware
// (gRPC), httpobs.NewHandler (stdlib net/http). Use
// tracing.StartClientSpan ONLY for outbound calls with NO wrapper (e.g. the
// document database). Never wrap a call with httpobs AND also open a manual
// StartClientSpan around it — that double-instruments the same request.
//
// # Boundary (ADR-007)
//
// This package does NOT create or own the *http.Client. The application builds
// its transport (including custom TLS/timeout/proxy) and passes it here; the
// helper wraps it and returns. It never dials and never closes. NewClient is a
// convenience that returns an *http.Client whose Transport is the wrapped one;
// pass the app's base transport so its TLS configuration is preserved.
//
// # Emitted telemetry
//
// otelhttp emits http.client.request.duration (seconds) with labels
// http.request.method, http.response.status_code, server.address, error.type
// (docs/metrics-contract.md), and creates a CLIENT span. The response body must
// be fully read and closed by the caller — the span ends on body close / EOF.
//
// # PII / cardinality guardrail (docs/metrics-contract.md)
//
// The three surfaces this package OWNS are kept bounded and PII-free:
//   - the duration metric labels never include url.path/url.query;
//   - the span name is bounded by default ("HTTP <METHOD>", e.g. "HTTP GET") and
//     never folds a concrete URL path into the name; and
//   - the URL recorded on a span NEVER carries the query string, the fragment,
//     userinfo or an opaque request target — url.full keeps scheme://host/path
//     of a hierarchical URL and only scheme://host of an opaque one.
//
// Enforced by tests.
//
// # Credentials in the URL (always, no opt-out)
//
// An API key in the query string (Google Gemini's ?key=, a pre-signed S3/GCS
// signature, any ?token=/?access_token=) would otherwise be exported verbatim to
// the collector, because url.full is a standard semconv attribute the
// instrumentation records from the request URL. NewTransport therefore hands the
// instrumentation a request whose URL has been stripped of query, fragment,
// userinfo and opaque target, and restores the full URL below it: the REQUEST ON THE WIRE IS
// UNCHANGED, and so are the propagation headers and the request-size accounting.
// This is behaviour, not an option — a credential in a span is never acceptable.
//
// The PATH is kept (it is what makes a span readable), so an identifier in the
// path still reaches the trace: redact that in the OTel Collector (transform
// processor), which is where path-shaped PII/cardinality redaction belongs.
// Inbound (NewHandler) needs nothing: the SERVER span records url.path and never
// the query string, so a ?code=/?state= callback is already safe.
//
// # No-op degradation (ADR-005)
//
// base nil -> http.DefaultTransport. With no MeterProvider the metric degrades to
// no-op. With no TracerProvider NO CLIENT span is produced (metric may still be
// recorded): the telemetry-enabled path MUST pass WithTracerProvider. Absent
// providers never make the helper panic or break the client. NewHandler degrades
// the same way and still serves the wrapped handler; a nil or panicking handler
// or base transport is the caller's, as with any middleware.
//
// # Inbound trace context (NewHandler, fail-closed)
//
// NewHandler IGNORES the inbound traceparent/tracestate by default and starts a
// new root trace per request; pass otel.GetTextMapPropagator() via
// WithPropagators to continue a trusted caller's trace. See NewHandler.
package httpobs
