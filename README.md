# lib-observability

`lib-observability` is Lerian's shared Go library for observability and telemetry. It provides a unified, OpenTelemetry-native instrumentation layer for tracing, metrics, structured logging, panic recovery with telemetry, and production-safe assertions — extracted from [`lib-commons`](https://github.com/LerianStudio/lib-commons) to give observability its own versioning, dependency footprint, and release cadence.

## What's in this library

### Telemetry bootstrap and tracing (`opentelemetry`)

Full OpenTelemetry SDK lifecycle management: OTLP/gRPC exporter setup for traces, metrics, and logs; `TracerProvider`, `MeterProvider`, and `LoggerProvider` construction via a single `NewTelemetry(cfg)` call; global provider opt-in with `ApplyGlobals()`; and graceful shutdown with `ShutdownTelemetry()`. Includes trace context propagation for HTTP, gRPC, and message queues (Kafka/Redpanda/RabbitMQ), span error/event recording helpers, struct-to-attribute conversion with automatic sensitive field redaction, and custom `SpanProcessor` implementations for context-carried attribute injection.

### Metrics (`opentelemetry/metrics`)

Thread-safe `MetricsFactory` with lazy instrument caching and a fluent builder API for Counters, Gauges, and Histograms. Provides `.WithLabels()` / `.WithAttributes()` chaining followed by `.Add()`, `.Set()`, or `.Record()` — all with explicit error returns. Includes pre-configured domain metric recorders (accounts, transactions, routes, operations) and system infrastructure gauges (CPU, memory). Ships a `NewNopFactory()` for tests and disabled-metrics environments.

### Structured logging (`log`)

A minimal, implementation-agnostic `Logger` interface with five methods (`Log`, `With`, `WithGroup`, `Enabled`, `Sync`), four severity levels, and typed `Field` constructors (`String`, `Int`, `Bool`, `Err`, `Any`). Includes a stdlib-based `GoLogger` with CWE-117 log-injection prevention, a `NopLogger` for tests, production-aware error sanitization (`SafeError`, `SanitizeExternalResponse`), and a generated mock for unit testing.

### Zap adapter with OTEL bridge (`zap`)

A [`zap`](https://github.com/uber-go/zap) adapter implementing the `Logger` interface, with automatic `trace_id` and `span_id` injection into every log entry. Bridges zap output to the OpenTelemetry Logs SDK via `otelzap`, enabling unified log collection through the OTLP pipeline. Supports environment-aware configuration (production, staging, development, local) and runtime log level adjustment.

### Panic recovery with telemetry (`runtime`)

Policy-driven panic recovery (`KeepRunning` / `CrashProcess`) with full observability integration: span event recording (`panic.recovered`), panic counter metrics (`panic_recovered_total`), structured logging, and optional external error reporter forwarding. Provides safe goroutine launchers (`SafeGo`, `SafeGoWithContext`) and `HandlePanicValue` for integration with HTTP/gRPC framework recovery middleware. Supports production mode for stack trace redaction.

### Production assertions with instrumentation (`assert`)

A context-scoped `Asserter` that validates domain invariants at runtime without panicking — every assertion failure returns an error, records a span event (`assertion.failed`), and increments the `assertion_failed_total` metric counter. Includes a predicates library for financial domain validation (decimal precision, balance sufficiency, transaction state transitions, debit/credit equality) alongside general-purpose checks (`NotNil`, `NotEmpty`, `NoError`, `ValidUUID`).

### Observability constants and context carriers

Shared OTEL attribute prefixes, metric names, event names, header constants (`Traceparent`, `Tracestate`), label sanitization (`SanitizeMetricLabel`), and sensitive field detection for cross-cutting redaction. Context carrier helpers (`ContextWithTracer`, `ContextWithMetricFactory`, `ContextWithLogger`, `ContextWithSpanAttributes`) for propagating observability primitives through `context.Context`.

### Redaction engine

A configurable `Redactor` with rule-based field processing supporting mask, hash (SHA-256), and drop actions. Applies automatically to span attributes via the `RedactingAttrBagSpanProcessor` and to struct-to-attribute conversion. Includes `ObfuscateStruct` for generic struct field obfuscation and integration with the sensitive field detection layer.

## Design principles

- **Explicit initialization** — no implicit global state; `NewTelemetry` + `ApplyGlobals` is opt-in
- **Nil-safe and no-op by default** — every factory and logger has a null-object variant for safe degradation
- **Errors over panics** — metric/builder operations return errors; assertions return errors instead of panicking
- **Redaction-first** — sensitive fields are masked in spans, logs, and attributes by default
- **Interface-driven** — `Logger`, `MetricsFactory`, `ErrorReporter`, and `DLQMetrics` are all interface-bound for testability

## Relationship to lib-commons

This library was extracted from `lib-commons` to decouple observability infrastructure from service primitives and data connectors. Services that previously imported `lib-commons` for telemetry can migrate to `lib-observability` for a lighter dependency graph. `lib-commons` will depend on `lib-observability` for its own instrumentation needs (database spans, streaming metrics, middleware telemetry).
