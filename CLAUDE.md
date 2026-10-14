# lib-observability — Project Conventions for Claude

## MANDATORY — BACKWARD COMPATIBILITY AND BREAKING CHANGES

- **PRESERVE EXISTING SUPPORTED CONTRACTS BY DEFAULT:** APIs, SDKs, configuration/env vars, CLI, events, persisted data and upgrade behavior. Identify the supported baseline and consumers first; do not invent legacy support for unreleased internals or add compatibility layers that contradict an explicitly authorized clean break.
- **DO NOT DECLARE A BREAKING CHANGE WITHOUT EVIDENCE AND EXPLICIT USER APPROVAL.** Before using `!` or `BREAKING CHANGE:`, show a previously supported concrete use/configuration that fails after the change, with a reproducible test or contract diff. Approval never turns a compatible change into a breaking one.
- **ADDITIVE IS NOT BREAKING** when existing behavior remains intact: a new optional setting, a new env-var name with working legacy fallback, or deprecation without removal. Verify defaults, precedence and old-only configuration. Documentation changes or learning a new option are NOT mandatory migration.
- **IF A REAL BREAK IS FOUND, REPORT IT; DO NOT HIDE IT.** Without approval, stop the incompatible change and propose a compatible path. Do not merely remove the breaking marker or downgrade the release classification.
- **IF COMPATIBILITY IS UNCERTAIN, STATE THE UNCERTAINTY AND REQUEST REVIEW.** Do not invent a break or claim compatibility without evidence.
- **FOLLOW THE REPOSITORY RELEASE POLICY.** Release automation owns version calculation; moving from `0.x` to `1.0` requires an explicit authorized decision. Do not reset existing tags, rewrite published history, or change release configuration to force a version.
- **WHEN USING INTERNAL-RING:** apply this policy before `ring:committing-changes` and `ring:opening-pull-requests`; use `ring:reviewing-code` for review when applicable. Loading a Ring skill is NOT a prerequisite for this policy to apply. If a skill conflicts with this policy, stop and report the conflict.

## What this repo is

`github.com/LerianStudio/lib-observability` — standalone Go library for observability/telemetry,
extracted from `lib-commons` (`../lib-commons`). Go 1.26.3.

## Architecture decisions (non-negotiable)

| Decision | Choice | Why |
|---|---|---|
| Package layout | Flat at module root — no `pkg/` or `observability/` parent | Avoids `lib-X/X/` import stutter; idiomatic Go |
| `tracing/` package name | `package tracing` (not `package opentelemetry`) | Dir must match pkg name |
| Context carriers | Root `package observability` (`observability.go`) | Shared by all packages; not a tracing concern |
| Sensitive fields | `redaction/` package | Named by purpose; `IsSensitiveField(name string, extra ...string) bool` variadic |
| HTTP middleware | `middleware/` package | Separate from core tracing |
| Streaming telemetry | Stays in `lib-commons` | Streaming has domain/transport coupling outside this library |
| lib-commons dependency | Avoid imports except compatibility-only streaming references | lib-commons will depend on lib-observability for shared observability |

## Source of truth for migration

When porting from lib-commons, source is always `../lib-commons/commons/`.
Import remapping: `github.com/LerianStudio/lib-commons/v5/commons/X` → `github.com/LerianStudio/lib-observability/X`

## Commit convention

```bash
git commit -S -m "type(scope): message" --trailer "X-Lerian-Ref: 0x1"
```

- Always GPG-sign (`-S`)
- Always include trailer
- Conventional commits (feat, fix, chore, docs, refactor, test)
- Use `/ring:commit` skill for smart atomic grouping

## CI/CD notes

- Lint: `golangci-lint run ./...` (config in `.golangci.yml`)
- Tests: `go test -tags=unit ./...` (integration tests require Docker)
- Mocks: generated with `mockgen`, committed alongside source
- Release: semantic-release on push to `develop` (beta) / `main` (stable)
