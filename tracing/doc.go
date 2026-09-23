// Package tracing provides OpenTelemetry SDK lifecycle management, trace context
// propagation for HTTP/gRPC/message queues, span error and event recording helpers,
// struct-to-attribute conversion with sensitive field redaction, and custom
// SpanProcessor implementations.
//
// # Instrumentation scope
//
// Signals this library emits itself — middleware spans, messaging spans, the
// transport duration instruments (HTTP, gRPC, messaging) — carry a fixed
// scope: the module path github.com/LerianStudio/lib-observability/v4 and the
// module version linked into the running binary, "(devel)" when the build
// carries none. Nothing configures it, because the scope names the code that
// produced the signal.
//
// The scope version changes on every release of this library, and so does the
// otel_scope_version label of the transport metric series. Aggregate those
// metrics without scope labels, e.g.
// sum by (http_request_method, http_response_status_code) (rate(...)), never
// by (otel_scope_version); grep recording rules and alerts for
// otel_scope_version before upgrading. The collector may drop the label.
//
// Signals the service emits carry the service's scope. That is Telemetry.Tracer
// and Telemetry.Meter, which take the scope as an argument, and also the
// carriers this library hands to service code: the tracer the HTTP and gRPC
// middleware put on the request context, and Telemetry.MetricsFactory. Those
// use TelemetryConfig.LibraryName exactly as configured, with no fallback, so a
// service metric keeps its series identity across upgrades of this library.
//
// # Build identity on the resource
//
// ServiceVersion is published as service.version and ServiceRevision, the full
// git SHA the binary was built from, as vcs.ref.head.revision on the resource of
// traces, metrics and logs; a blank ServiceRevision omits the attribute. Both
// come from the binary, not the environment: a service sets them at link time
// (-ldflags "-X main.version=... -X main.revision=...") and passes them to
// NewTelemetry explicitly.
package tracing
