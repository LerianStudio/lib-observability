# lib-observability — Project Conventions for Claude

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
