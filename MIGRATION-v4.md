# Migrating to lib-observability v4

v4 has one job: **stop this library's major version from propagating through
the fleet.**

It is not a feature release. Nothing was added to what the library observes.
What changed is the *shape of its public boundary*, so that the next major —
and there will be one, the next time Fiber moves — costs the fleet an import
line instead of a rewrite.

---

## Why v4 exists

### The majors were never about the logger

| version | date | commit that forced the major |
|---|---|---|
| v2.0.0 | 2026-07-20 | `feat(core)!: migrate to Fiber v3` |
| v3.0.0 | 2026-08-10 | `refactor(core)!: decouple Fiber v3 from tracing core` |

Both are `middleware/` concerns. Neither touched logging. Yet each one
rewrote hundreds of files in midaz that never imported `middleware/` at all.

### Why a middleware change reached those files

A Go module's major version lives in its **import path**. Bumping to `/v3`
renames every package in the module at once, `log/` included. That alone is a
mechanical rename — annoying, not expensive.

The expensive part is that the rename also changes the **identity of the
types**. Go matches the types inside a method signature *nominally*:

```go
// v2/log/log.go and v3/log/log.go — byte-for-byte identical, same MD5
type Level uint8
type Field struct { Key string; Value any }

type Logger interface {
    Log(ctx context.Context, level Level, msg string, fields ...Field)
}
```

`v2/log.Level` and `v3/log.Level` are **different types**. So a consumer could
never write its own equivalent interface:

```go
// lib-commons wants to depend on "something that logs", not on a version.
// This does NOT work: nothing satisfies it except a type built from
// lib-commons' own Level and Field, which no logger in the fleet is.
type Logger interface {
    Log(ctx context.Context, level Level, msg string, fields ...Field)
}
```

Its only option was to import `lib-observability/v2/log` and name
`log.Logger` — and by importing it, inherit its major. That is the propagation
mechanism, and it is why a Fiber upgrade in `middleware/` costs 481 files in
midaz.

`With(fields ...Field) Logger` made it strictly worse: a **self-returning**
method is unsatisfiable from outside the declaring package by construction,
because a foreign package has no way to name the return type.

### What v4 changes

Every parameter position that accepts a logger or a metrics recorder is now
declared with **universal types only** — `context.Context`, `int`, `string`,
`any`, `error` — or with a *local one-method interface* built from those.
Conversion happens inside the library, at the call.

```go
// v4: any logger, from any version, forever. Including one declared in a
// package that has never imported lib-observability.
func SafeGo(logger interface {
    Log(ctx context.Context, level int, msg string, fields ...any)
}, name string, policy PanicPolicy, fn func())
```

The payoff is not for leaf services directly. It is for the **libraries in
between**. `lib-commons` has 76 non-test files importing
`lib-observability/v2/log` purely to *name* `log.Logger` in its own
signatures. After v4 it can declare that interface locally, import nothing,
and stop taking a major every time this library takes one — which also
dissolves the diamond that currently forces midaz onto one exact
lib-observability major shared with lib-commons.

A regression test (`boundary_test.go`) walks the exported API with `go/ast`
and fails if any logger- or recorder-shaped parameter names a type defined by
this module. The class of defect cannot come back silently.

---

## What did NOT change

These were deliberate, because changing them would have cost the fleet
thousands of edits for no decoupling benefit:

- **The level scale stays Error=0, Warn=1, Info=2, Debug=3.** It is inverted
  from `log/slog` (Debug=-4 … Error=8). Adopting slog's scale would have
  silently inverted the severity of 1419 call sites. Not done, not proposed.
- **`log.Field` and its constructors** (`String`, `Int`, `Bool`, `Err`,
  `Any`) are unchanged and still preferred. They now travel through an `...any`
  variadic and are recognized on arrival.
- **The level constant names** are unchanged. They are now *untyped*, which is
  what lets existing call sites keep compiling.
- **Return types stay rich.** `NewLoggerFromContext` and
  `NewTrackingFromContext` still return `log.Logger`. Returning a rich value is
  free — a caller can always assign it to a narrower local interface. Only
  *parameters* were widened.
- `sqlobs`, `redisobs`, `httpobs` were already universal at the boundary
  (OpenTelemetry types only, from a shared stable v1 module). Untouched.

---

## From / to

| v3 | v4 | breaking? |
|---|---|---|
| `Logger.Log(ctx, Level, string, ...Field)` | `Logger.Log(ctx, int, string, ...any)` | only for **implementers** |
| `Logger.With(...Field) Logger` | `Logger.With(...any) Logger` | only for implementers |
| `Logger.Enabled(Level) bool` | `Logger.Enabled(int) bool` | only for implementers |
| `const LevelError Level = iota` | `const LevelError = iota` (untyped) | no |
| — | `log.LevelName(int) string` | new |
| — | `log.LevelValid(int) bool` | new |
| — | `log.Fields(...any) []Field` | new |
| — | `log.Universal` (one-method interface) | new |
| — | `log.Adapt(Universal) Logger` | new |
| `log.SafeError(Logger, …)` | `log.SafeError(Universal, …)` | no |
| `runtime.Logger` naming `log.Level`/`log.Field` | `runtime.Logger` = `Log(ctx, int, string, ...any)` | only for implementers |
| `assert.Logger` naming `log.Level`/`log.Field` | same universal shape | only for implementers |
| `metrics.NewMetricsFactory(meter, log.Logger)` | `metrics.NewMetricsFactory(meter, metrics.Logger)` | no |
| — | `metrics.Recorder` + `AddCounter`/`SetGauge`/`RecordHistogram` **on `*MetricsFactory`** | new |
| `runtime.InitPanicMetrics(*metrics.MetricsFactory, …)` | `runtime.InitPanicMetrics(runtime.Recorder, …)` | no |
| `assert.InitAssertionMetrics(*metrics.MetricsFactory)` | `assert.InitAssertionMetrics(assert.Recorder)` | no |
| `observability.GetCPUUsage(ctx, *metrics.MetricsFactory)` | `observability.GetCPUUsage(ctx, SystemRecorder)` | no |
| `observability.ContextWithLogger(ctx, log.Logger)` | `observability.ContextWithLogger(ctx, log.Universal)` | no |
| `middleware.WithCustomLogger(obslog.Logger)` | `middleware.WithCustomLogger(obslog.Universal)` | no |
| `zap.Slog(logpkg.Logger)` | `zap.Slog(logpkg.Universal)` | no |
| `tracing.TelemetryConfig.Logger log.Logger` | `… log.Universal` | only if you *read* the field and call `With`/`Enabled`/`Sync` |
| module `…/v3` | module `…/v4` | **yes — import path** |

Note the shape of that table: almost every row is "no". Because the level
constants became untyped and `Field` flows through `...any`, **call sites
compile unchanged**. The breaking rows are the import path, and the handful of
types that *implement* the logger interface.

---

## Before / after

### Logging — unchanged

```go
// identical in v3 and v4
logger.Log(ctx, libLog.LevelError, "failed to post transaction",
    libLog.String("ledger_id", ledgerID),
    libLog.Err(err),
)
```

All 5713 structured `.Log(ctx, …)` call sites across the fleet keep compiling.
Only the import line moves.

### Passing a `[]Field` — drop the `...`

```go
// v3
logger.Log(ctx, libLog.LevelInfo, "request finished", fields...)

// v4 — pass the slice as one argument; log.Fields flattens []Field in place,
// so the rendered output is identical.
logger.Log(ctx, libLog.LevelInfo, "request finished", fields)
```

### An explicitly-typed `Level` variable — convert at the call

```go
// v3
var lvl libLog.Level = libLog.LevelWarn
logger.Log(ctx, lvl, "msg")

// v4
logger.Log(ctx, int(lvl), "msg")
// or just drop the annotation: `lvl := libLog.LevelWarn` is already an int.
```

### Implementing the interface — the one real break

```go
// v3 test double
func (l *stubLogger) Log(ctx context.Context, level libLog.Level, msg string, fields ...libLog.Field) {
    l.entries = append(l.entries, msg)
}

// v4
func (l *stubLogger) Log(ctx context.Context, level int, msg string, fields ...any) {
    // if you need typed fields:
    for _, f := range libLog.Fields(fields...) { _ = f.Key }
    l.entries = append(l.entries, msg)
}
```

Same for `With(...any) Logger` and `Enabled(int) bool`.

### Emitting a metric without importing lib-observability

```go
// v4: declare this in YOUR package. Import nothing.
type Recorder interface {
    AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error
}

func (s *Service) record(ctx context.Context, r Recorder) error {
    return r.AddCounter(ctx, "transactions_processed", "…", "1", nil, 1)
}
```

`*metrics.MetricsFactory` satisfies this **directly** — the methods are on the
concrete type, not on a wrapper — so the caller passes `tl.MetricsFactory`
with no adapter.

### Accepting a logger without importing lib-observability

```go
// v4: declare this in YOUR package. Import nothing.
type Logger interface {
    Log(ctx context.Context, level int, msg string, fields ...any)
}

func New(logger Logger) *Service { return &Service{logger: logger} }
```

`*log.GoLogger`, the zap adapter, `log.NewNop()`, and the value returned by
`observability.NewTrackingFromContext` all satisfy it, from any version.

---

## Cost per repo

Files importing lib-observability, measured on `origin/develop` of each repo
on 2026-08-28 (`vendor/` and stray worktrees excluded):

| repo | non-test | test | **sed-only** | manual |
|---|---|---|---|---|
| midaz | 370 | 108 | 478 | 6 |
| tenant-manager | 312 | 114 | 426 | 5 |
| plugin-access-manager | 115 | 108 | 223 | 9 |
| lib-commons | 76 | 65 | 141 | **32** |
| lib-streaming | 22 | 43 | 65 | 8 |
| lib-systemplane | 26 | 5 | 31 | 5 |
| lib-license-go | 12 | 3 | 15 | 4 |
| lib-service-discovery | 6 | 7 | 13 | 1 |
| lib-auth | 5 | 4 | 9 | 1 |
| **total** | **944** | **457** | **1401** | **71** |

`sed-only` is the import-path rename — fully mechanical, below. `manual` is
the upper bound of files that may need a human look: `fields...` spreads (11
fleet-wide, 7 of them in lib-commons), explicitly-typed `log.Level` variables
(~60, most of which are config plumbing that needs no change at all), and the
13 types that implement the logger interface (11 of them test doubles).

For comparison: the v2→v3 bump cost midaz 481 files of the same mechanical
rename **plus** a full re-verification, because nothing guaranteed the
semantics were unchanged. Here they are guaranteed by the compiler — if it
builds, the call is the same call.

---

## The mechanical step

Run in each repo, on a branch:

```bash
# 1. rename the import path (covers v2 and v3 consumers in one pass)
grep -rl 'LerianStudio/lib-observability/v[23]' --include='*.go' . \
  | grep -v '/vendor/' \
  | xargs sed -i -E 's#(LerianStudio/lib-observability)/v[23]#\1/v4#g'

# 2. bump the module requirement
go mod edit -dropreplace github.com/LerianStudio/lib-observability/v3 2>/dev/null
go get github.com/LerianStudio/lib-observability/v4@latest
go mod tidy

# 3. let the compiler find the residue — it is a short list
go build ./... 2>&1 | tee /tmp/v4-residue.txt
go vet -tags unit ./...
```

Step 3 reports exactly two error shapes, and both have a one-line fix:

```
cannot use fields (variable of type []log.Field) as []any value
    -> drop the `...`

cannot use lvl (variable of type log.Level) as int value
    -> wrap in int(...)
```

plus, for the few files that implement the interface, `wrong type for method
Log` — apply the pattern in *Before / after* above.

For a repo with no `fields...` spread and no `log.Level` variables — which is
midaz, tenant-manager, PAM, and every leaf service — step 1 and step 2 are the
whole migration.

## Suggested order

`lib-commons` first (it is the one with real manual work, and everything else
depends on it), then `lib-streaming` / `lib-systemplane` / `lib-auth` /
`lib-service-discovery` / `lib-license-go` in any order, then the leaf
services: midaz, tenant-manager, plugin-access-manager.

While migrating `lib-commons`, take the opportunity the release exists for:
replace its `liblog.Logger` parameters with a locally-declared one-method
interface and drop the `lib-observability` import entirely. That is the change
that makes this the last major of this library the fleet has to care about.
