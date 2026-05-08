# lib-observability — Claude Code Project Context

## What this repo is

`github.com/LerianStudio/lib-observability` — new standalone Go library extracting observability/telemetry from `lib-commons` (`../lib-commons`). Flat package layout at module root (no `pkg/` stutter). Go 1.25.9.

## Architecture decisions (final, non-negotiable)

| Decision | Choice | Rationale |
|---|---|---|
| Package naming | `tracing/` → `package tracing` | Dir/pkg mismatch is a papercut that compounds |
| Context carriers | Root `package observability` (`observability.go`) | Only observability helpers; `tracing/`, `log/`, `metrics/` consume it |
| Sensitive fields | `redaction/` package | Named by purpose, not scope; `IsSensitiveField(name string, extra ...string) bool` variadic |
| HTTP middleware | `middleware/` package | Separate from core tracing primitives |
| Streaming telemetry | `streaming/` package | Pure telemetry — no franz-go/kafka dep needed |

## Current repo state (as of last session)

### Branch: `feat/migrate-from-lib-commons` (open PR #2 on GitHub)

**Already merged to develop (PR #1):** bootstrap — CI/CD, go.mod, Makefile, scaffold dirs

**In PR #2 — already committed:**
- `constants/` — OTEL attrs, metric names, event names, headers, obfuscation
- `redaction/` — `IsSensitiveField`, `DefaultSensitiveFields` (variadic extension)
- `observability.go` + `system.go` — context carriers + CPU/mem helpers (root package)
- `log/` — Logger interface, GoLogger (CWE-117), NopLogger, sanitizer
- `metrics/` — MetricsFactory, fluent Counter/Gauge/Histogram builders, domain recorders
- `runtime/` — panic recovery, PanicMetrics, SafeGo, ErrorReporter
- `assert/` — Asserter, AssertionMetrics, financial predicates
- `tracing/` — OpenTelemetry SDK bootstrap, span helpers, propagation, redaction engine (renamed from `package opentelemetry`)
- `zap/` — zap adapter with OTEL bridge, trace_id/span_id injection

**lib-commons circular dep:** fully removed — `tracing/otel.go` uses self-contained `isStrictEnvironment()` instead of `commons.EffectiveSecurityTier`.

## What still needs to be done (in PR #2 — session was interrupted here)

### 1. Apply CodeRabbit fixes (7 valid issues)

| File | Fix |
|---|---|
| `constants/headers.go:9` | `HeaderTraceparent="traceparent"` (lowercase W3C); `HeaderTraceparentPascal="Traceparent"` (keep distinct) — currently both are `"Traceparent"` (duplicate bug) |
| `zap/zap.go:193` | `Debug/Info/Warn/Error` bypass `sanitizeConsoleMsg` — CWE-117 gap; route through sanitizer |
| `assert/predicates.go:345` | `TransactionHasOperations(["   "])` returns true — add blank-after-trim check |
| `tracing/obfuscation.go:136` | `fieldRegex==nil` falls back to `IsSensitiveField` instead of "match any" — logic bug |
| `tracing/obfuscation.go:191` | HMAC fallback uses plain SHA256 — use deterministic HMAC fallback key for consistency |
| `tracing/otel.go:536` | `sanitizeSpanMessage` only redacts first Bearer/Basic token — loop to redact all |
| `zap/injector.go:45` | `strings.TrimSpace(OTelLibraryName)==""` validation — whitespace passes currently |

### 2. Migrate HTTP/gRPC middleware → `middleware/` package

Source: `../lib-commons/commons/net/http/`
Files to port:
- `withTelemetry.go` → `middleware/telemetry.go` (`TelemetryMiddleware`, gRPC unary interceptor)
- `withTelemetry_helpers.go` → `middleware/helpers.go` (URL sanitization, route exclusion, `resolveGRPCRequestID`)
- `withTelemetry_metrics.go` → `middleware/metrics.go` (background system metrics collector)
- `context_span.go` → `middleware/context_span.go` (`SetHandlerSpanAttributes`, `SetTenantSpanAttribute`)

Import remapping: `commons/opentelemetry` → `github.com/LerianStudio/lib-observability/tracing`, etc.
Package: `package middleware`
Fiber dep already in go.mod.

### 3. Migrate streaming telemetry → `streaming/` package

Source: `../lib-commons/commons/streaming/`
Files to port (telemetry only — NO franz-go dependency):
- `emit_span.go` → `streaming/emit_span.go`
- `metrics.go` → `streaming/metrics.go`
- `metrics_recorders.go` → `streaming/metrics_recorders.go`

These only import `log` and `metrics` (OTEL) — clean extraction.
Package: `package streaming`

### 4. Port test files from lib-commons

Source files → target:
- `../lib-commons/commons/opentelemetry/*_test.go` (7 files) → `tracing/`
- `../lib-commons/commons/log/*_test.go` (3 files) → `log/`
- `../lib-commons/commons/zap/*_test.go` (2 files) → `zap/`
- `../lib-commons/commons/runtime/*_test.go` (7 files) → `runtime/`
- `../lib-commons/commons/assert/*_test.go` (4 files) → `assert/`
- `../lib-commons/commons/net/http/withTelemetry*_test.go` (2 files) → `middleware/`

Import remapping in all test files: `lib-commons/v5/commons/X` → `lib-observability/X`

### 5. Generate `log/log_mock.go`

The `//go:generate` directive points to a non-existent generated file. Run:
```bash
go install go.uber.org/mock/mockgen@latest
cd /home/gbrecci/Documents/dev/projects/lerianstudio/lib-observability
mockgen --destination=log/log_mock.go --package=log github.com/LerianStudio/lib-observability/log Logger
```

### 6. After all above: validate

```bash
go build ./...
go vet ./...
go test -tags=unit ./...
go mod tidy
```

## PR #2 context

- URL: https://github.com/LerianStudio/lib-observability/pull/2
- Reviewer comments: CodeRabbit (technical) + Gandalf (strategic, score 7/10)
- Gandalf's score path: 7→8 (declare phases) → 9 (middleware+streaming+tests) → 10 (docs+CI coverage)
- User decision: do it all in this PR — no phases, complete migration

## How to call Gandalf (Lerian AI team member)

```bash
RESP=$(curl -s -X POST http://gandalf.heron-justitia.ts.net:18792/task \
  -H "Content-Type: application/json" \
  -d '{"action":"ask","message":"YOUR QUESTION HERE","context":"lib-observability migration from lib-commons"}')
TASK_ID=$(echo $RESP | jq -r .task_id)
# Poll:
for i in $(seq 1 60); do
  RESULT=$(curl -s http://gandalf.heron-justitia.ts.net:18792/task/$TASK_ID)
  STATUS=$(echo $RESULT | jq -r .status)
  [ "$STATUS" != "processing" ] && echo $RESULT | jq -r .response && break
  sleep 5
done
```

Use Gandalf for: Lerian environment context, team decisions, architecture choices specific to Midaz services.
Ask the user directly for: scope decisions, priority changes, anything Gandalf can't answer clearly.

## Key file references

| What | Path |
|---|---|
| Source lib | `../lib-commons/commons/` |
| HTTP middleware source | `../lib-commons/commons/net/http/` |
| Streaming telemetry source | `../lib-commons/commons/streaming/{emit_span,metrics,metrics_recorders}.go` |
| Test files source | Per package above |
| PR #2 | https://github.com/LerianStudio/lib-observability/pull/2 |

## Commit convention

```bash
git commit -S -m "feat(scope): message" --trailer "X-Lerian-Ref: 0x1"
```
Always sign (`-S`), always include trailer. Conventional commits.
Use `/ring:commit` skill for smart grouping.
