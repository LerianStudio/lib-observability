# Lib-observability Changelog

## [3.1.0](https://github.com/LerianStudio/lib-observability/releases/tag/v3.1.0)

Features:
- Introduced `redisobs.Setup` which returns a cleanup function for pool-stat registrations, enhancing Redis observability. (@rodrigodh)
- Added `sqlobs.Setup` for one-call SQL pool instrumentation, simplifying the setup process for SQL observability. (@rodrigodh)

Fixes:
- Addressed an issue in metrics where connectivity was not preserved during Setup and added guards for typed nils. (@fredcamaral)
- Ensured Redis metrics are instrumented before tracing to maintain accurate observability. (@rodrigodh)
- Resolved middleware issues by satisfying `errorlint` and `inamedparam` in `asFiberError`, and preventing typed-nil `*fiber.Error` from shadowing valid errors in joined chains. (@fredcamaral)
- Implemented guards for typed-nil `*fiber.Error` matched by `errors.As` to prevent erroneous error handling. (@fredcamaral)

Improvements:
- Enhanced the SQL observability documentation by adding a nil-guard to the Setup example and softening the rationale for empty-DSN to allow safe recreation. (@fredcamaral)

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v3.0.0...v3.1.0)

---

## [3.0.0](https://github.com/LerianStudio/lib-observability/releases/tag/v3.0.0)

Features:
- Bump Go module path to `/v3` for the `v3.0.0` release. (@fredcamaral)
- Add HTTP client instrumentation wrapper for `httpobs`. (@gauchito91)
- Introduce native RED/runtime metrics and provide instrumentation helpers for databases, caches, and queues. (@gauchito91)
- Add `StartClientSpan` helper for tracing. (@gauchito91)

Fixes:
- Ensure HTTP/gRPC error handling is safe for typed-nil values and maintains observability integrity. (@fredcamaral)
- Prevent HTTP identity leakage in middleware. (@fredcamaral)
- Name HTTP spans by route template instead of raw path. (@gauchito91)
- Resolve `golangci-lint` findings in middleware. (@fredcamaral)
- Address review feedback on typed-nil events and baggage restoration in middleware. (@fredcamaral)
- Attribute typed-nil handler errors and improve stream-test access logging in middleware. (@fredcamaral)

Improvements:
- Correct SDK cardinality-limit statement in tenant-id gap note for `grpcmiddleware`. (@fredcamaral)
- Document URI lifecycle on `NewRequestInfo` in middleware. (@fredcamaral)
- Clarify `httpobs` provider-dependency and client ownership. (@gauchito91)
- Document `httpobs` and `StartClientSpan` for client-side outbound operations. (@gauchito91)
- Add pre-development artifacts for span-kind-client-helpers. (@gauchito91)
- Document wrapper precedence principle for span kind. (@gauchito91)
- Implement `ENV_NAME` fallback and strict `ParseBool` in core documentation. (@gauchito91)
- Read telemetry configuration from environment variables instead of hard-coding endpoints. (@gauchito91)
- Fix insecure-exporter production example and missing imports in core documentation. (@gauchito91)
- Fix error handling in usage snippets in core documentation. (@gauchito91)
- Add instrumentation guide for native metrics and wrappers. (@gauchito91)
- Bump `grpc` dependency for CVE and clarify `sqlobs` pool and gRPC documentation reference. (@gauchito91)
- Decouple Fiber `v3` from tracing core, gRPC, and system collector. (@gauchito91)
- Satisfy `wsl_v5` whitespace requirements before gRPC interceptor return in middleware. (@gauchito91)
- Extend gRPC span ownership in middleware tests. (@gauchito91)
- Address CodeRabbit review feedback in middleware tests. (@gauchito91)
- Enforce Go module major tags in CI release process. (@fredcamaral)
- Renew `trivyignore` exception for `GO-2026-5932`. (@fredcamaral)

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v2.1.2...v3.0.0)

---

## [Unreleased]


Features:

- **`WithHTTPErrorHandling`**: finalizes a Fiber handler's error before outer
  logging/telemetry middleware inspect the response, invoking the app's
  effective `ErrorHandler` exactly once. Registering it is a correctness
  requirement, not an optional add-on - without it, a valid, non-nil error
  whose `Unwrap` chain hits a typed-nil (`errors.Join(fiber.NewError(400,
  ...), typedNil)` is the reproduced case) reaches Fiber's own default
  `ErrorHandler`, which panics calling `err.Error()` unconditionally,
  underneath any panic-recovery middleware registered above it. Two ordering
  rules apply: (1) no middleware that can return a non-nil error after
  `c.Next()` may sit between the observability middleware
  (`WithHTTPLogging`/`WithTelemetry`) and this one, or the app `ErrorHandler`
  runs a second time; (2) panic recovery must be present somewhere in the
  chain (either side of this middleware works) - it normalizes returned
  errors, it does not recover panics itself.

BREAKING BEHAVIOR (not yet released):

- **`http.route` on pre-routing refusals**: a request that never matches a
  registered route (Fiber's catch-all 404, and any middleware that rejects
  before routing) no longer reports `http.route="/"` on spans - the attribute
  is omitted entirely, and the HTTP access log's `http_path` field reports
  `/{unmatched}` instead. Update dashboards/alerts that group or filter on
  `http.route="/"` to also account for the missing-attribute and
  `/{unmatched}` cases.
- **`WithObfuscationDisabled` / `LOG_OBFUSCATION_DISABLED` are now no-ops**:
  HTTP request bodies are never captured by access logging, so there is
  nothing left to obfuscate or to disable obfuscation for. Callers still
  passing this option or setting this env var see no effect; safe to remove.
- **`RequestInfo.Body`, `.Referer`, `.Username` are always empty/`"-"`**:
  these fields are retained for source compatibility but never populated
  from the request. Code reading them for anything beyond the CLF access-log
  line must be updated.
- **A 5xx handler error now records the OTel semconv `exception` event**
  (`exception.type` carries the handler error's original Go type,
  `exception.message` the sanitized message), instead of the custom
  `http.handler.error` event - Tempo/Jaeger and other APM backends index
  specifically on the semconv keys, so the earlier custom-named event with
  the same information was invisible to them. The
  custom `http.handler.error` event is unchanged for <500 responses (a
  mapped 4xx must never produce an `exception` event or ERROR span status,
  per semconv). Span status itself is still gated on status code >=500
  either way.
- **HTTP correlation IDs (`X-Request-Id`) are now capped at 128 characters
  and non-ASCII bytes are stripped**, both new relative to the last release:
  an ID longer than 128 characters is silently truncated, and an ID that is
  entirely non-ASCII (or reduces to empty after stripping) is replaced with a
  generated UUID rather than passed through. Punctuation such as `:`, `/`,
  `+`, `=` is preserved untouched.
- **Inbound trace-context extraction is now opt-in, for BOTH HTTP and gRPC**:
  set `TelemetryConfig.TrustInboundTraceContext = true` to have `WithTelemetry`
  (HTTP) and `WithTelemetryInterceptor` (gRPC) honor an inbound
  `traceparent`/`tracestate` header/metadata; the default is `false`
  (fail-closed) for both. Previously, HTTP honored inbound trace context
  unconditionally, and gRPC honored it whenever the caller's User-Agent
  matched an internal-Lerian-service pattern - a spoofable heuristic, not an
  authenticated trust boundary, so it never actually restricted anything a
  malicious caller couldn't bypass. **A service on an internal mesh that
  relied on that gRPC User-Agent heuristic to join traces must now set
  `TrustInboundTraceContext = true` explicitly, or its gRPC spans silently
  become trace roots instead of joining the caller's trace.** The `tenant.id`
  baggage member is always stripped on extraction regardless of this setting
  or transport (HTTP, gRPC, or queue) - see "Known limitations" below for the
  one caller-controlled tenant field this does NOT cover. As part of this
  fix, gRPC baggage propagation itself - previously a complete no-op due to a
  key-casing mismatch between grpc-go's lowercased metadata and the
  propagation library's canonicalizing lookup - now actually works for every
  baggage member.
- **The HTTP access logger's default `Level` changed from `LevelError` to
  `LevelInfo`**: a caller of `WithHTTPLogging()`/`WithGrpcLogging()` with no
  `WithCustomLogger` option previously logged only 5xx responses (a
  pre-existing bug: the zero-value default emitted nothing else); it now
  logs every request, consistent with the documented behavior a custom
  logger set to `LevelInfo` always had. Expect access-log volume to increase
  for any caller relying on the old default.

Known limitations (documented, not addressed by this release):

- **`grpcmiddleware.ResolveTenantIDFromGRPC` still reads a caller-controlled
  `tenant-id` gRPC metadata field directly** and stamps it onto spans and
  the `rpc.server.duration` metric's `tenant.id` label - unlike the
  `tenant.id` OTel baggage member, which is now unconditionally stripped
  from every inbound carrier (see above). This is a pre-existing mechanism;
  closing it is a separate, deliberately deferred product decision (some
  internal consumers may depend on it as a cross-service hint), not a gap
  introduced by this release.

API-stability notes for consumers pinning to internals (both from this
release, neither part of the documented public contract but worth flagging
explicitly since they can silently change behavior or fail to compile):

- **`log.Level` constant VALUES changed** (`LevelError` was `5`, is now `0`;
  `LevelWarn`/`LevelInfo`/`LevelDebug` shift the same way) - a pre-existing
  bug where the four `Level` constants shared an `iota` block with five
  unrelated string constants declared before them is fixed, restoring the
  values the doc comment always claimed. Any consumer that persisted a
  `Level` as a bare integer, or compared against a numeric literal instead
  of the named constant, has its meaning silently flipped. Named-constant
  usage (`log.LevelError`, etc.) is unaffected.
- **`middleware.RequestInfo` gained an unexported field (`start`)**: a
  positional (unkeyed) struct literal for `RequestInfo` from outside the
  package no longer compiles (Go requires every field, in order, for an
  unkeyed literal, and an external package cannot supply an unexported one).
  Construct it via `NewRequestInfo` instead, as intended.

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v2.1.2...HEAD)

---

## [2.1.2](https://github.com/LerianStudio/lib-observability/releases/tag/v2.1.2)

Fixes:

- Restored the trusted `tenant.id` baggage member verbatim instead of rebuilding it via `NewMember`. (@fredcamaral)
- Addressed review findings related to typed-nil `originalErr`, `exception.type`, and nil guards in middleware. (@fredcamaral)
- Satisfied `dogsled` and `ST1008` lint requirements on error handling helpers in middleware. (@fredcamaral)
- Implemented typed-nil-safe error handling and ensured streaming-safe access logs as part of the `v2` backport for middleware. (@fredcamaral)
- Enhanced fiber middleware safety in version `v2`. (@fredcamaral)

[Compare changes](https://github.com/LerianStudio/lib-observability/compare/v2.1.1...v2.1.2)

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

