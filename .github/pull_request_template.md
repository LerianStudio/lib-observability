<table border="0" cellspacing="0" cellpadding="0">
  <tr>
    <td><img src="https://github.com/LerianStudio.png" width="72" alt="Lerian" /></td>
    <td><h1>Lib Observability</h1></td>
  </tr>
</table>

---

## Description

<!-- Summarize what this PR changes and why. Mention the package(s) affected
     (log, metrics, tracing, middleware, redaction, zap, runtime, assert, ...). -->

## Type of Change

- [ ] `feat`: New feature or capability
- [ ] `fix`: Bug fix
- [ ] `perf`: Performance improvement
- [ ] `refactor`: Internal restructuring with no behavior change
- [ ] `docs`: Documentation only (README, docs/, inline comments)
- [ ] `style`: Formatting, whitespace, naming (no logic change)
- [ ] `test`: Adding or updating tests
- [ ] `ci`: CI pipeline or workflow changes
- [ ] `build`: Build system, Go module dependencies
- [ ] `chore`: Maintenance, config, tooling
- [ ] `revert`: Reverts a previous commit
- [ ] `BREAKING CHANGE`: Consumers must update their integration

## Breaking Changes

<!-- If applicable, describe exactly what breaks (public API signatures,
     exported types, default behaviors, configuration keys) and how downstream
     services should migrate. Remove this section if not applicable. -->

None.

## Testing

- [ ] `make test` passes
- [ ] `make test-int` passes if integration paths are exercised
- [ ] `make lint` passes
- [ ] Coverage threshold respected (see Go Combined Analysis)

**Test evidence / Actions run:** <!-- Optional: link to a CI run or screenshot -->

## Architectural Checklist

- [ ] No `panic()` in production paths — uses `assert` helpers or wrapped errors
- [ ] Timestamps use `time.Now().UTC()`
- [ ] Errors wrapped with `%w`
- [ ] Public API additions documented (godoc + README if user-facing)
- [ ] No leaking of context or goroutines (instrumentation must be safe under load)
- [ ] Backwards-compatible by default; breaking changes flagged above
- [ ] No `lib-commons` import (circular — see CLAUDE.md)

## Related Issues

Closes #
