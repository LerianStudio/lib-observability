# Lib-observability Changelog

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

