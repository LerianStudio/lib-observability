# Lib-observability Changelog

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

