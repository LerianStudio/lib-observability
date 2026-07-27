// Package httpobs provides a thin, nil-safe helper that wraps an outbound HTTP
// transport with OpenTelemetry client instrumentation. Every outbound request is
// classified as a call to an external dependency (span kind CLIENT) and emits
// http.client.request.duration, giving RED visibility of the dependencies a
// service calls (identity provider, BACEN/SPB, tenant-manager, etc.).
//
// It is a thin wrapper over go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp:
// httpobs applies the library's corporate defaults and does not reimplement span,
// metric, or context-propagation logic.
//
// # Precedence (ADR-006)
//
// PREFER this wrapper for outbound HTTP. For each outbound call type, use the
// dedicated wrapper — SQL: sqlobs, Redis/Valkey: redisobs, HTTP client: httpobs,
// messaging: messagingobs, inbound server: middleware/grpcmiddleware. Use
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
// The two surfaces this package OWNS are kept bounded and PII-free:
//   - the duration metric labels never include url.path/url.query; and
//   - the span name is bounded by default ("HTTP <METHOD>", e.g. "HTTP GET") and
//     never folds a concrete URL path into the name.
//
// Enforced by tests. Note (ADR-008): otelhttp always sets url.full (the raw
// request URL, path and query) as a standard semconv attribute on the CLIENT
// span, and OTel-Go offers no supported hook to remove it. If outbound URLs may
// carry identifiers/PII in the path or query, redact url.full in the OTel
// Collector (transform processor) — that is where PII/cardinality redaction of
// span attributes belongs.
//
// # No-op degradation (ADR-005)
//
// base nil -> http.DefaultTransport. With no MeterProvider the metric degrades to
// no-op. With no TracerProvider NO CLIENT span is produced (metric may still be
// recorded): the telemetry-enabled path MUST pass WithTracerProvider. The helper
// never panics and never breaks the client.
package httpobs
