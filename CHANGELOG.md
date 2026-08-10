# Lib-observability Changelog

## [Unreleased]

Fixes:
- Added `middleware.WithHTTPErrorHandling`, a dedicated middleware that finalizes a Fiber handler error exactly once before outer logging/telemetry middleware inspect the response. Without it, a valid but unsafe-to-stringify handler error (an `errors.Join` wrapping a typed-nil is the reproduced case) reached Fiber's default `ErrorHandler`, which calls `err.Error()` unconditionally and panics underneath any panic-recovery middleware registered above it. Register it after `WithHTTPLogging`/`WithTelemetry` and before application routes.
- Stopped the HTTP access-log middleware from calling `c.Body()` unconditionally on every request. The unconditional read forced fasthttp to buffer the full request body before the downstream handler could consume it as a stream, breaking large/streamed uploads (`RequestBodyStream()` returned `nil` afterward). Access logging no longer inspects the request body at all; `RequestInfo.Body`, request-body obfuscation, and `WithObfuscationDisabled` are retained only for source compatibility and are now no-ops. `Referer`/`Username` are fixed `"-"` placeholders instead of being parsed from the request. The response-side body size is now read via `IsBodyStream`-aware `Content-Length` fallback instead of forcing a streamed response into memory, and the CLF size field renders `-` (not a misleading `-1`) when the size cannot be determined.
- Added typed-nil-safe helpers (`log.IsNil`, `log.SafeErrorMessage`, `log.IsSafeToStringify`) and applied them everywhere an error crosses into a sink with an unguarded `.Error()` call: `tracing.HandleSpanError`, `tracing.HandleSpanBusinessErrorEvent`, `assert.NoError`, and `log.GoLogger`'s field sanitizer. A valid, non-nil error whose `Unwrap` chain hits a typed-nil (the standard library's `errors.Join` is the textbook case) previously panicked when stringified by any of these; they now degrade to a safe fallback message instead.
- Fixed the `log.Level` enum: `LevelError`/`LevelWarn`/`LevelInfo`/`LevelDebug` shared a single `const` block with four unrelated string constants, so `iota` numbered `LevelError` as `5` instead of `0` - silently breaking every `>=` comparison against it, including `GoLogger.Enabled`. An uninitialized `&GoLogger{}` (the HTTP access-log middleware's default when no custom logger is supplied) is now correctly enabled at `LevelInfo` instead of emitting nothing.
- On a >=500 handler error, the HTTP telemetry middleware now calls `span.RecordError`, producing the OpenTelemetry semantic-convention `exception` span event that APM backends (Tempo, Jaeger) index on; a <500 handler error keeps recording the library's own custom `http.handler.error` event. The HTTP access log's `error` field, and the discarded-rogue-error diagnostic emitted by `WithHTTPErrorHandling`, are now rendered through the same `tracing.ErrorMessage` sanitizer (redaction + length cap) the span uses, instead of a raw, unguarded `.Error()` call.
- Fixed inbound OTel baggage extraction silently overwriting an already-seeded `tenant.id`: `propagation.Baggage.Extract` replaces the whole baggage value on the context rather than merging into it, so an inbound `baggage` header that carried other members but no `tenant.id` erased an in-process tenant identity that survives fine when the header is absent entirely. `tracing.ExtractTraceContext` - the single funnel every extraction path (HTTP, gRPC, queue) goes through - now captures a pre-existing `tenant.id` before extraction, strips any `tenant.id` the inbound carrier claims (a caller must never be able to forge the value stamped on every span), and restores the captured value afterward if one existed.
- Replaced the HTTP telemetry middleware's inbound trace-context extraction gate, previously a User-Agent pattern match (spoofable by any caller that set the header, never a real trust boundary), with the explicit, fail-closed `tracing.TelemetryConfig.TrustInboundTraceContext` flag (default `false`). A deployment that wants to continue an inbound trace over HTTP - typically a service behind a trusted internal mesh or gateway - now opts in explicitly. The gRPC server interceptor's own User-Agent gate is unchanged.
- Fixed `http.route` / access-log route attribution using a status-code heuristic (`effective status == 404 AND route == "/" AND path != "/"`) to detect an unmatched request, which misclassified a handler that legitimately returns 404 on a matched route, and a non-404 response on an unmatched path. Route presence is now read directly from Fiber's own routing state (`c.Matched()`).
- Fixed a span-lifecycle ordering bug: registering `EndTracingSpans` alongside `WithTelemetry` let Fiber's LIFO middleware unwinding end the span before `WithTelemetry` applied the route template, status, and error attributes - measured as spans landing with the bare method as their name (`"GET"` instead of `"GET /orders/:id"`). `WithTelemetry` now marks a span it created as owned; `EndTracingSpans` skips ending an owned span and lets the owning middleware finalize and end it.
- Fixed three sites that dereferenced a typed-nil `Logger` (a non-nil interface wrapping a nil concrete logger) instead of treating it as absent: `assert.New`, `observability.resolveLogger`, and `observability.NewLoggerFromContext` all now check `log.IsNil` instead of `!= nil`. This mattered for `WithHTTPErrorHandling`'s own panic-recovery path (`invokeErrorHandlerSafely`, `diagnoseDiscardedRogueError`), which calls `NewTrackingFromContext(...).Log(...)` - an app wired with a typed-nil logger turned error recovery itself into a new panic source.
- Hardened `normalizeRequestID` (the `X-Request-Id` header/correlation-ID sanitizer): it previously stripped only CR/LF/NUL and had no length cap, so a tab byte was echoed back raw in the response header and an unbounded caller-supplied ID passed through in full. It now allow-lists printable ASCII (0x20-0x7E) and caps the result at 128 characters.
- The HTTP access log now derives its entry's log level from the effective status (`httpAccessLogLevel`): 5xx logs at Error, 4xx at Warn, everything else at Info. Previously every access-log line - including a 500 - was emitted at Info, so a request-serving failure did not stand out from routine traffic in a level-filtered log view.

Known limitations:
- gRPC inbound baggage extraction remains a no-op on this v2 line: `metadata.MD` lowercases every key (mandated by HTTP/2), so `propagation.Baggage`'s canonicalizing lookup never finds a gRPC-propagated `baggage` header, and no case-remapping was added here to fix it. Tenant.id protection is unaffected - the strip/restore in `tracing.ExtractTraceContext` covers every extraction path (HTTP, gRPC, queue) regardless of whether baggage actually reaches it. The gRPC server interceptor also keeps its own, pre-existing User-Agent trust gate, unlike the HTTP path's `TrustInboundTraceContext` flag above - wiring the gRPC baggage rail through a spoofable gate would make the v2 line's posture worse, not better, so this divergence from the v3 line is deliberate, not an oversight.

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v2.1.1...HEAD)

---

## [2.1.1](https://github.com/LerianStudio/lib-observability/releases/tag/v2.1.1)

Fixes:
- Resolved an issue where the access-log route was being resolved before routing was completed, ensuring proper middleware execution. (@fredcamaral)
- Addressed a problem with HTTP server spans by filtering out request identity information at the start, enhancing privacy. (@fredcamaral)
- Prevented HTTP identity leakage within middleware, safeguarding sensitive information. (@fredcamaral)

Improvements:
- Renewed the expired `GO-2026-5932` entry in the `trivyignore` file to maintain up-to-date security practices. (@fredcamaral)
- Enforced Go module major tags in the CI release process to ensure consistency and adherence to versioning standards. (@fredcamaral)

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v2.1.0...v2.1.1)

---

## [2.1.0](https://github.com/LerianStudio/lib-observability/releases/tag/v2.1.0)

Features:
- Add `zap.Slog` accessor for `slog`-compatible consumers. (@rodrigodh)

Fixes:
- Bump `google.golang.org/grpc` to `v1.82.1`. (@rodrigodh)

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v2.0.0...v2.1.0)

---

## [2.0.0](https://github.com/LerianStudio/lib-observability/releases/tag/v2.0.0)

Features:
- Migrate to Fiber `v3`, initiating a new major version `/v2`. (@rodrigodh)

Fixes:
- Update the import path in the README example to use the `/v2` path. (@gandalf-at-lerian)
- Upgrade `golang.org/x/text` to `v0.39.0` to address a security vulnerability. (@rodrigodh)

Improvements:
- Accept the unmaintained `x/crypto/openpgp` issue `GO-2026-5932` in the CI process. (@rodrigodh)

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v1.1.0...v2.0.0)

---

## [1.1.0](https://github.com/LerianStudio/lib-observability/releases/tag/v1.1.0)

- **Features:**
  - Add `tenant.id` as a shared observability attribute.
  - Record `http.server.request.duration` in `WithTelemetry`.
  - Skip probe and scrape paths by default; add `WithExcludedRoutes` tests.
  - Read `tenant.id` from OTel baggage for spans and logs.

- **Fixes:**
  - Persist trimmed endpoint and treat null/nil baggage as empty in tracing.
  - Add `http_latency_ms` field to `WithHTTPLogging` access log.
  - Include baggage-propagated `tenant.id` in `http.server.request.duration` metric.
  - Sanitize `tenant.id` from baggage before seeding spans.
  - Infer insecure exporter for scheme-less OTLP endpoint.

- **Improvements:**
  - Promote `context.id` to shared `AttrKeyContextID` constant.
  - Move tenant constants to constants package.
  - Align duration histogram buckets with OTel advisory.
  - Normalize `http.request.method` to OTel known set.
  - Omit `http.route` for unmatched 404 requests.

Contributors: @brunognovaes, @fredcamaral, @gandalf-at-lerian, @gauchito91, @lerian-studio, @qnen.

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v1.0.1...v1.1.0)

---

## [1.0.1](https://github.com/LerianStudio/lib-observability/releases/tag/v1.0.1)

- Improvements:
  - Remove internal Gandalf call instructions.

Contributors: @gandalf-at-lerian, @lerian-studio.

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v1.0.0...v1.0.1)

---

## [1.0.0](https://github.com/LerianStudio/lib-observability/releases/tag/v1.0.0)

- **Features:**
  - Added logging middleware to enhance observability.
  - Migrated HTTP/gRPC telemetry middleware from `lib-commons`.
  - Removed streaming package to streamline the library.
  - Migrated observability packages from `lib-commons`.
  - Bootstrapped repository with CI/CD, linting, and release configuration.

- **Fixes:**
  - Addressed review feedback to improve code quality.
  - Decoupled error field key from level strings in logging.
  - Added `nolint` directives for scaffolded symbols in streaming.
  - Handled `GOBIN` in `gosec` fallback for CI.
  - Pinned `go_version` to `1.25.9` to satisfy `go.mod` toolchain requirement.

- **Improvements:**
  - Raised unit test coverage for `lib-observability`.
  - Aligned workflows with shared workflows `@v1.28.12` boilerplate.
  - Prepared `lib-observability` stable gates for CI.
  - Ported unit test suite from `lib-commons`.
  - Generated Logger mock via `mockgen`.

Contributors: @bedatty, @gandalf-at-lerian, @qnen.

[View all changes](https://github.com/LerianStudio/lib-observability/commits/v1.0.0)

