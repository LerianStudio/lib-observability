# Lib-observability Changelog

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

