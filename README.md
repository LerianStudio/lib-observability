# lib-observability

`lib-observability` is Lerian's shared Go library for observability and telemetry. It provides a unified, OpenTelemetry-native instrumentation layer for tracing, metrics, structured logging, panic recovery with telemetry, and production-safe assertions — extracted from [`lib-commons`](https://github.com/LerianStudio/lib-commons) to give observability its own versioning, dependency footprint, and release cadence.

## What's in this library

### Telemetry bootstrap and tracing (`tracing`)

Full OpenTelemetry SDK lifecycle management: OTLP/gRPC exporter setup for traces, metrics, and logs; `TracerProvider`, `MeterProvider`, and `LoggerProvider` construction via a single `NewTelemetry(cfg)` call; global provider opt-in with `ApplyGlobals()`; and graceful shutdown with `ShutdownTelemetry()`. Includes trace context propagation for HTTP, gRPC, and message queues (Kafka/Redpanda/RabbitMQ), span error/event recording helpers, struct-to-attribute conversion with automatic sensitive field redaction, and custom `SpanProcessor` implementations for context-carried attribute injection.

### Metrics (`metrics`)

Thread-safe `MetricsFactory` with lazy instrument caching and a fluent builder API for Counters, Gauges, and Histograms. Provides `.WithLabels()` / `.WithAttributes()` chaining followed by `.Add()`, `.Set()`, or `.Record()` — all with explicit error returns. Includes pre-configured domain metric recorders (accounts, transactions, routes, operations) and system infrastructure gauges (CPU, memory). Ships a `NewNopFactory()` for tests and disabled-metrics environments.

### Outbound HTTP client instrumentation (`httpobs`)

A thin, nil-safe wrapper over `otelhttp` that turns an outbound HTTP transport into an instrumented one: every outbound request is classified as a call to an external dependency (span kind `CLIENT`) and emits `http.client.request.duration` (seconds). `NewTransport(base, opts...)` wraps the transport the app already built (preserving its TLS/timeout/proxy config); `NewClient(base, opts...)` is a convenience returning a ready `*http.Client`. Bounded span name by default (`HTTP <METHOD>`), no-op when telemetry is off. See "Outbound call instrumentation" below.

### Manual client-span helper (`tracing.StartClientSpan`)

For outbound calls that have **no** dedicated wrapper (e.g. the document database, custom RPC/SDK), `tracing.StartClientSpan(ctx, tracer, name, opts...)` starts a span already classified as `CLIENT`, so hand-rolled network hops stop defaulting to `INTERNAL`. The `CLIENT` kind is an overridable default. See "Outbound call instrumentation" below.

### Structured logging (`log`)

A minimal, implementation-agnostic `Logger` interface with five methods (`Log`, `With`, `WithGroup`, `Enabled`, `Sync`), four severity levels, and typed `Field` constructors (`String`, `Int`, `Bool`, `Err`, `Any`). Includes a stdlib-based `GoLogger` with CWE-117 log-injection prevention, a `NopLogger` for tests, production-aware error sanitization (`SafeError`, `SanitizeExternalResponse`), and a generated mock for unit testing.

### Zap adapter with OTEL bridge (`zap`)

A [`zap`](https://github.com/uber-go/zap) adapter implementing the `Logger` interface, with automatic `trace_id` and `span_id` injection into every log entry. Bridges zap output to the OpenTelemetry Logs SDK via `otelzap`, enabling unified log collection through the OTLP pipeline. Supports environment-aware configuration (production, staging, development, local) and runtime log level adjustment.

### Panic recovery with telemetry (`runtime`)

Policy-driven panic recovery (`KeepRunning` / `CrashProcess`) with full observability integration: span event recording (`panic.recovered`), panic counter metrics (`panic_recovered_total`), structured logging, and optional external error reporter forwarding. Provides safe goroutine launchers (`SafeGo`, `SafeGoWithContext`) and `HandlePanicValue` for integration with HTTP/gRPC framework recovery middleware. Supports production mode for stack trace redaction.

### Production assertions with instrumentation (`assert`)

A context-scoped `Asserter` that validates domain invariants at runtime without panicking — every assertion failure returns an error, records a span event (`assertion.failed`), and increments the `assertion_failed_total` metric counter. Includes a predicates library for financial domain validation (decimal precision, balance sufficiency, transaction state transitions, debit/credit equality) alongside general-purpose checks (`NotNil`, `NotEmpty`, `NoError`, `ValidUUID`).

### Observability constants and context carriers

Shared OTEL attribute prefixes, metric names, event names, header constants (`traceparent`, `Traceparent`, `Tracestate`), label sanitization (`SanitizeMetricLabel`), and sensitive field detection for cross-cutting redaction. Context carrier helpers (`ContextWithTracer`, `ContextWithMetricFactory`, `ContextWithLogger`, `ContextWithSpanAttributes`) for propagating observability primitives through `context.Context`.

### Redaction engine

A configurable `Redactor` with rule-based field processing supporting mask, hash (SHA-256), and drop actions. Applies automatically to span attributes via the `RedactingAttrBagSpanProcessor` and to struct-to-attribute conversion. Includes `ObfuscateStruct` for generic struct field obfuscation and integration with the sensitive field detection layer.

## Design principles

- **Explicit initialization** — no implicit global state; `NewTelemetry` + `ApplyGlobals` is opt-in
- **Nil-safe and no-op by default** — every factory and logger has a null-object variant for safe degradation
- **Errors over panics** — metric/builder operations return errors; assertions return errors instead of panicking
- **Redaction-first** — sensitive fields are masked in spans, logs, and attributes by default
- **Interface-driven** — `Logger`, `MetricsFactory`, `ErrorReporter`, and `DLQMetrics` are all interface-bound for testability

## Outbound call instrumentation — span kind precedence

Outbound calls (crossing a process/network boundary: DB, cache, HTTP, another service) must be classified as `CLIENT` in telemetry. Inbound requests are `SERVER`; purely in-process work is `INTERNAL`. A raw `tracer.Start(ctx, name)` defaults to `INTERNAL`, so an outbound call instrumented by hand is **mis-classified** — which hides the latency/error of your external dependencies and inflates the `INTERNAL` series.

**Rule of precedence — for humans and AI assistants:**

1. **Always prefer the dedicated wrapper.** It sets the correct span kind (and metric) automatically, so you never have to remember `WithSpanKind`.

   | Outbound call type | Use this | Span kind |
   |---|---|---|
   | SQL database | `sqlobs` | CLIENT |
   | Redis / Valkey | `redisobs` | CLIENT |
   | HTTP client | `httpobs` | CLIENT |
   | Messaging (produce/consume) | `messagingobs` | PRODUCER / CONSUMER |
   | Inbound HTTP / gRPC (server) | `middleware` / `grpcmiddleware` | SERVER |

2. **Only when no wrapper exists**, use `tracing.StartClientSpan` by hand — e.g. the document database (no stable driver instrumentation today) or a custom RPC/SDK call.

3. **Never double-instrument.** Do not wrap a call with a wrapper **and** also open a manual `StartClientSpan` around the same call — that produces two spans for one operation. Use the wrapper for wrapped call types; use `StartClientSpan` only for the rest.

### Example — HTTP client (use the wrapper)

Wrap the transport your app already built so its custom TLS/timeout/proxy config is preserved. `WithTracerProvider` is required for the CLIENT span (otherwise only the metric is emitted).

```go
import (
    "net/http"

    "github.com/LerianStudio/lib-observability/v2/httpobs"
)

func newInstrumentedClient(baseTransport http.RoundTripper) *http.Client {
    return httpobs.NewClient(baseTransport,
        httpobs.WithMeterProvider(meterProvider),
        httpobs.WithTracerProvider(tracerProvider), // required for the CLIENT span
    )
}

// When the app builds its own *http.Client, wrap only the transport:
//   client.Transport = httpobs.NewTransport(baseTransport,
//       httpobs.WithMeterProvider(meterProvider),
//       httpobs.WithTracerProvider(tracerProvider))
```

Migration note: once the transport is wrapped, remove any manual `tracer.Start(...)` you previously opened around the HTTP call (no double-instrumentation). The caller must fully read and close the response body — the span ends on body close.

### Example — no wrapper (use StartClientSpan)

For an outbound call with no wrapper — e.g. the document database — replace the hand-rolled `tracer.Start(...)` so the span is `CLIENT`:

```go
import "github.com/LerianStudio/lib-observability/v2/tracing"

// Before: outbound Mongo call rendered as INTERNAL
//   ctx, span := tracer.Start(ctx, "mongodb.find_holder")
// After: classified as an external-dependency call (CLIENT)
ctx, span := tracing.StartClientSpan(ctx, tracer, "mongodb.find_holder")
defer span.End()
// ... perform the document-database call ...
```

> **PII in outbound URLs:** `httpobs` keeps the duration metric labels and the span name bounded and PII-free. However `otelhttp` always records `url.full` (the raw request URL, including path and query) as a standard attribute on the client span, and OpenTelemetry-Go offers no supported hook to strip it. If your outbound URLs can carry identifiers/PII in the path or query, redact `url.full` in the OTel Collector (transform processor) — that is where span-attribute PII/cardinality redaction belongs.

## Tenant ID propagation

The HTTP and gRPC middleware automatically read a tenant identifier from the request and propagate it through telemetry as the `tenant.id` attribute / log field.

### Wire protocol

- **HTTP:** canonical header `X-Tenant-Id`. No aliases.
- **gRPC:** canonical metadata key `tenant-id`. No aliases.

Values are normalized (trimmed, control chars stripped) and dropped silently when empty or longer than 128 bytes to bound telemetry cardinality.

### Where it shows up

| Signal | How it gets there | Action required by caller |
|---|---|---|
| Logs | `WithHTTPLogging` / `WithGrpcLogging` add `tenant.id` as a structured field. | None |
| Traces | The `AttrBagSpanProcessor` (registered by default in `tracing.NewTelemetry`) copies the request attribute bag onto every span at start. | None |
| `http.server.request.duration` (built-in metric) | `WithTelemetry` adds `tenant.id` to the histogram when present in the request bag. Label is omitted when no tenant was supplied so non-tenant traffic does not split the time series. | None |
| Custom application metrics | Not automatic. Metric labels are a high-impact cardinality decision left to the caller. | Use `middleware.RequestAttributes(ctx)` to opt in per metric. |

Example for custom metrics:

```go
import "github.com/LerianStudio/lib-observability/v2/middleware"

counter, _ := factory.Counter("orders.created")
_ = counter.
    WithAttributes(middleware.RequestAttributes(ctx)...).
    Add(ctx, 1)
```

### Trust boundary

The header is **client-controlled**. The middleware treats it as an observability hint, not an authenticated identifier:

- Run authentication (JWT, mTLS, etc.) **before** these middlewares apply effects you care about.
- If your auth layer resolves the real tenant from a signed credential, call `observability.ContextWithSpanAttributes(ctx, attribute.String("tenant.id", real))` from the handler. The attribute bag is deduplicated by key with last-wins semantics, so the override surfaces in subsequent logs/traces and replaces the header-supplied value.

## Relationship to lib-commons

This library was extracted from `lib-commons` to decouple observability infrastructure from service primitives and data connectors. Services that previously imported `lib-commons` for telemetry can migrate to `lib-observability` for a lighter dependency graph. `lib-commons` will depend on `lib-observability` for its own instrumentation needs (database spans, streaming metrics, middleware telemetry).
