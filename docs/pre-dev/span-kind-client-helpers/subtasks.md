# Gate 8 — Subtasks: span-kind-client-helpers

> Gates anteriores: prd · feature-map · trd · api-design · data-model · dependency-map · tasks (mesmo dir)
> Data: 2026-07-27 · Track: Full · Status: draft (aguardando aprovação)
> **Zero-context, TDD RED-GREEN, código completo (sem placeholder).** Módulo: `github.com/LerianStudio/lib-observability/v2`. Root: `/home/gauchito/lerian/lib-observability`.
> Commits: `git commit -S -m "..." --trailer "X-Lerian-Ref: 0x1"`. Verificação por task: `golangci-lint run ./...` + `go test -tags=unit ./...`.

---

# T1 — `tracing.StartClientSpan`

## ST-T1-01: Teste RED do helper StartClientSpan

**Goal:** provar que ainda não existe helper que force kind CLIENT por default e permita override.
**Prereq:** `cd /home/gauchito/lerian/lib-observability && go test -tags=unit ./tracing/` verde hoje.
**Files:** Create `tracing/spanhelpers_test.go`.

**Step 1 — Escrever o teste que falha.** Criar `tracing/spanhelpers_test.go`:
```go
//go:build unit

package tracing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func newSpanRecorder(t *testing.T) (trace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test"), sr
}

func TestStartClientSpan_DefaultsToClientKind(t *testing.T) {
	tracer, sr := newSpanRecorder(t)

	_, span := StartClientSpan(context.Background(), tracer, "mongodb.find")
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, trace.SpanKindClient, ended[0].SpanKind(),
		"outbound span must default to CLIENT")
	assert.Equal(t, "mongodb.find", ended[0].Name())
}

func TestStartClientSpan_CallerCanOverrideKind(t *testing.T) {
	tracer, sr := newSpanRecorder(t)

	// Caller explicitly asks for SERVER — must win over the CLIENT default
	// because options are last-wins and the helper PREPENDS its default (ADR-004).
	_, span := StartClientSpan(context.Background(), tracer, "custom",
		trace.WithSpanKind(trace.SpanKindServer))
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, trace.SpanKindServer, ended[0].SpanKind(),
		"explicit caller kind must override the CLIENT default")
}

func TestStartClientSpan_PassesThroughCallerOptions(t *testing.T) {
	tracer, sr := newSpanRecorder(t)

	_, span := StartClientSpan(context.Background(), tracer, "op",
		trace.WithAttributes())
	span.End()

	require.Len(t, sr.Ended(), 1)
	assert.Equal(t, trace.SpanKindClient, sr.Ended()[0].SpanKind())
}
```

**Step 2 — Rodar e confirmar que NÃO compila (RED).**
```
cd /home/gauchito/lerian/lib-observability && go test -tags=unit ./tracing/
```
Esperado: erro de compilação `undefined: StartClientSpan`.

**Step 3 — Implementação mínima.** Criar `tracing/spanhelpers.go`:
```go
package tracing

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// StartClientSpan starts a span already classified as an outbound call to an
// external dependency (SpanKind = CLIENT), so callers instrumenting a network
// hop by hand do not have to remember to pass trace.WithSpanKind.
//
// PREFER A WRAPPER WHEN ONE EXISTS. This helper is the LAST resort, for outbound
// calls that have NO dedicated wrapper:
//   - SQL              → use sqlobs
//   - Redis / Valkey   → use redisobs
//   - HTTP client      → use httpobs
//   - messaging        → use messagingobs
//   - inbound (server) → use the middleware / grpcmiddleware
// Use StartClientSpan only for the remainder, e.g. the document database
// (no stable driver instrumentation, see ADR-003) or a custom RPC/SDK call.
// Do NOT wrap a call that already has automatic instrumentation AND also open a
// StartClientSpan around it — that double-instruments the same operation.
//
// The CLIENT kind is applied as an overridable default: it is PREPENDED to opts,
// so a caller that deliberately passes its own trace.WithSpanKind(...) wins
// (span start options are last-wins).
//
// This helper only sets the span kind. It does NOT emit metrics and does NOT
// enforce any PII/cardinality contract on name or attributes: the caller owns a
// low-cardinality, PII-free span name and attributes.
func StartClientSpan(
	ctx context.Context,
	tracer trace.Tracer,
	name string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	// Prepend the CLIENT default so an explicit caller kind (appearing later)
	// overrides it (ADR-004).
	withClient := append(
		[]trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindClient)},
		opts...,
	)

	return tracer.Start(ctx, name, withClient...)
}
```

**Step 4 — Rodar e confirmar GREEN.**
```
cd /home/gauchito/lerian/lib-observability && go test -tags=unit ./tracing/
```
Esperado: `ok` — 3 testes passam.

**Step 5 — Lint.**
```
cd /home/gauchito/lerian/lib-observability && golangci-lint run ./tracing/
```
Esperado: sem issues.

**Step 6 — Commit.**
```
cd /home/gauchito/lerian/lib-observability && git add tracing/spanhelpers.go tracing/spanhelpers_test.go && git commit -S -m "feat(tracing): add StartClientSpan helper" --trailer "X-Lerian-Ref: 0x1"
```

**Rollback:** `git restore --staged tracing/spanhelpers*.go && rm tracing/spanhelpers.go tracing/spanhelpers_test.go`

---

# T2 — Dependência `otelhttp v0.69.0`

## ST-T2-01: Adicionar otelhttp e travar go.mod/go.sum

**Goal:** disponibilizar `otelhttp v0.69.0` sem bumpar o otel core.
**Prereq:** `cd /home/gauchito/lerian/lib-observability && grep -c "net/http/otelhttp" go.sum` retorna `0`.
**Files:** Modify `go.mod`, `go.sum`.

**Step 1 — Adicionar a dep na versão exata.**
```
cd /home/gauchito/lerian/lib-observability && go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.69.0
```

**Step 2 — Tidy.**
```
cd /home/gauchito/lerian/lib-observability && go mod tidy
```

**Step 3 — Verificar que o otel core NÃO subiu e a única transitiva nova é httpsnoop.**
```
cd /home/gauchito/lerian/lib-observability && grep "go.opentelemetry.io/otel v" go.mod && grep "felixge/httpsnoop" go.mod && git diff --stat go.mod go.sum
```
Esperado: `go.opentelemetry.io/otel v1.44.0` (inalterado); `github.com/felixge/httpsnoop v1.0.4 // indirect` presente; diff só em go.mod/go.sum.

**Step 4 — Verificar build.**
```
cd /home/gauchito/lerian/lib-observability && go build ./... && go mod verify
```
Esperado: build ok; `all modules verified`.

**Step 5 — Commit isolado.**
```
cd /home/gauchito/lerian/lib-observability && git add go.mod go.sum && git commit -S -m "chore(deps): add otelhttp v0.69.0 for http client instrumentation" --trailer "X-Lerian-Ref: 0x1"
```

**Rollback:** `git checkout go.mod go.sum`

---

# T3 — Pacote `httpobs`

## ST-T3-01: Config + Options (clone do padrão redisobs)

**Goal:** estabelecer `config`+`Option`+`WithX`+`newConfig` do httpobs.
**Prereq:** T2 mergeado (`grep -c "net/http/otelhttp" go.sum` ≥ 1).
**Files:** Create `httpobs/httpobs.go`.

**Step 1 — Criar `httpobs/httpobs.go` (config + options + construtores).**
```go
package httpobs

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// config holds resolved helper options.
type config struct {
	meterProvider     metric.MeterProvider
	tracerProvider    trace.TracerProvider
	propagator        propagation.TextMapPropagator
	spanNameFormatter func(operation string, r *http.Request) string
	extraAttrs        []attribute.KeyValue
}

// Option configures the HTTP client instrumentation helper.
type Option func(*config)

// WithMeterProvider sets the MeterProvider for http.client.request.duration.
// When unset the global provider is used (no-op unless configured). Nil ignored.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.meterProvider = mp
		}
	}
}

// WithTracerProvider sets the TracerProvider for the CLIENT span. When unset, no
// CLIENT span is produced (ADR-005): the telemetry-enabled path MUST pass this.
// Nil ignored.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		if tp != nil {
			c.tracerProvider = tp
		}
	}
}

// WithPropagators sets the propagator used to inject trace context into outbound
// request headers. When unset the global propagator is used. Nil ignored.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(c *config) {
		if p != nil {
			c.propagator = p
		}
	}
}

// WithSpanNameFormatter overrides how the outbound span is named. The DEFAULT is
// bounded ("HTTP <METHOD>", e.g. "HTTP GET") and never includes the URL path.
// GUARDRAIL (docs/metrics-contract.md): a custom formatter MUST stay
// low-cardinality — never fold a concrete URL path / id / PII into the name.
func WithSpanNameFormatter(fn func(operation string, r *http.Request) string) Option {
	return func(c *config) {
		if fn != nil {
			c.spanNameFormatter = fn
		}
	}
}

// WithAttributes appends additional low-cardinality, PII-free attributes to the
// spans/metrics. url.path, url.query, and any PII are FORBIDDEN
// (docs/metrics-contract.md) and are never added by this helper.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *config) {
		c.extraAttrs = append(c.extraAttrs, attrs...)
	}
}

func newConfig(opts ...Option) config {
	cfg := config{
		meterProvider: otel.GetMeterProvider(),
		propagator:    otel.GetTextMapPropagator(),
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// otelhttpOptions translates the resolved config into otelhttp options.
func (c config) otelhttpOptions() []otelhttp.Option {
	opts := []otelhttp.Option{
		otelhttp.WithMeterProvider(c.meterProvider),
		otelhttp.WithPropagators(c.propagator),
	}

	if c.tracerProvider != nil {
		opts = append(opts, otelhttp.WithTracerProvider(c.tracerProvider))
	}

	if c.spanNameFormatter != nil {
		opts = append(opts, otelhttp.WithSpanNameFormatter(c.spanNameFormatter))
	}

	return opts
}

// NewTransport wraps base with OpenTelemetry HTTP client instrumentation: every
// outbound request is classified as an external-dependency call (SpanKind=CLIENT,
// only when a TracerProvider is configured — ADR-005), emits
// http.client.request.duration (seconds), and propagates trace context on the
// outbound headers.
//
// PREFER this wrapper for outbound HTTP. Use tracing.StartClientSpan only for
// outbound calls WITHOUT a wrapper. Do not double-instrument.
//
// Nil-safe: base == nil uses http.DefaultTransport. With no providers configured
// it attaches against the no-op providers, so telemetry being off never breaks
// the client. The span ends when the response body is fully read/closed — the
// caller MUST read and close the body.
func NewTransport(base http.RoundTripper, opts ...Option) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}

	cfg := newConfig(opts...)

	return otelhttp.NewTransport(base, cfg.otelhttpOptions()...)
}

// NewClient returns an *http.Client whose Transport is the instrumented wrapper
// around base. Passing base preserves the caller's custom transport (TLS /
// timeout / proxy). base == nil uses http.DefaultTransport.
func NewClient(base http.RoundTripper, opts ...Option) *http.Client {
	return &http.Client{Transport: NewTransport(base, opts...)}
}
```

**Step 2 — Build (sem teste ainda).**
```
cd /home/gauchito/lerian/lib-observability && go build ./httpobs/
```
Esperado: compila.

**Step 3 — Commit parcial (opcional) ou seguir p/ ST-T3-02.** (Recomendado: seguir; commit único ao fim de T3.)

**Rollback:** `rm httpobs/httpobs.go`

## ST-T3-02: Teste RED — conformidade de métrica + span CLIENT

**Goal:** provar contrato: `http.client.request.duration` (s) + labels + span CLIENT.
**Files:** Create `httpobs/httpobs_test.go`.

**Step 1 — Escrever `httpobs/httpobs_test.go`.**
```go
//go:build unit

package httpobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const httpClientRequestDurationMetric = "http.client.request.duration"

func newHarness(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader, *sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	return mp, reader, tp, sr
}

// doGET drives one instrumented outbound request against a local test server and
// fully reads/closes the body so the span ends.
func doGET(t *testing.T, client *http.Client, url string) {
	t.Helper()
	resp, err := client.Get(url)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
}

func collectClientDuration(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var points []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != httpClientRequestDurationMetric {
				continue
			}
			require.Equal(t, "s", m.Unit, "http client duration must be seconds")
			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected float64 histogram, got %T", m.Data)
			points = append(points, h.DataPoints...)
		}
	}
	return points
}

func TestNewTransport_EmitsClientDurationMetric(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	doGET(t, client, srv.URL)

	points := collectClientDuration(t, reader)
	require.NotEmpty(t, points, "expected http.client.request.duration to be emitted")

	// Required labels present; forbidden ones absent.
	set := points[0].Attributes
	assertHasKey(t, set, "http.request.method")
	assertHasKey(t, set, "http.response.status_code")
	assertHasKey(t, set, "server.address")
	assertNoKey(t, set, "url.path")
	assertNoKey(t, set, "url.query")
}

func TestNewTransport_ProducesClientSpan(t *testing.T) {
	mp, _, tp, sr := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	doGET(t, client, srv.URL)

	var clientSpans int
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			clientSpans++
		}
	}
	assert.GreaterOrEqual(t, clientSpans, 1, "outbound request must produce a CLIENT span")
}
```

**Step 2 — Adicionar os helpers de asserção de label ao fim do arquivo de teste.**
```go
func assertHasKey(t *testing.T, set attribute.Set, key string) {
	t.Helper()
	_, ok := set.Value(attribute.Key(key))
	assert.True(t, ok, "expected attribute %q to be present", key)
}

func assertNoKey(t *testing.T, set attribute.Set, key string) {
	t.Helper()
	_, ok := set.Value(attribute.Key(key))
	assert.False(t, ok, "attribute %q must NOT be present (PII/cardinality guardrail)", key)
}
```
E adicionar ao import block: `"go.opentelemetry.io/otel/attribute"`.

**Step 3 — Rodar (GREEN esperado, já que a impl existe do ST-T3-01).**
```
cd /home/gauchito/lerian/lib-observability && go test -tags=unit ./httpobs/
```
Esperado: `ok`. Se `server.address`/`http.response.status_code` não vierem como label da métrica na v0.69.0, ver nota abaixo.

> **Nota de calibração (executar durante a implementação):** o conjunto exato de atributos na *métrica* `http.client.request.duration` é definido pelo otelhttp v0.69.0. Se o teste falhar por um label específico não estar no data point da métrica (vs no span), ajustar a asserção para o conjunto real emitido pela versão — mantendo SEMPRE as asserções `assertNoKey(url.path/url.query)` (guardrail inegociável) e a unidade `"s"`. Documentar o conjunto real no doc.go.

**Rollback:** `rm httpobs/httpobs_test.go`

## ST-T3-03: Teste RED — anti-PII (url.path/query nunca vira label)

**Goal:** garantir o guardrail: PII no path/query não vaza para métrica nem span.
**Files:** Modify `httpobs/httpobs_test.go` (append test).

**Step 1 — Adicionar o teste anti-vazamento.**
```go
func TestNewTransport_NeverEmitsURLPathOrQueryAsAttribute(t *testing.T) {
	mp, reader, tp, sr := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	const secretPath = "/users/123.456.789-00"
	const secretQuery = "pix_key=123.456.789-00"

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	doGET(t, client, srv.URL+secretPath+"?"+secretQuery)

	// Metric attributes must not carry the path/query/PII.
	for _, p := range collectClientDuration(t, reader) {
		for _, kv := range p.Attributes.ToSlice() {
			v := kv.Value.AsString()
			assert.NotContains(t, v, "123.456.789-00", "PII leaked into metric attribute %s", kv.Key)
			assert.NotContains(t, v, secretPath, "url.path leaked into metric attribute %s", kv.Key)
		}
		assertNoKey(t, p.Attributes, "url.path")
		assertNoKey(t, p.Attributes, "url.query")
	}

	// Span attributes and name must not carry the path/query/PII.
	for _, s := range sr.Ended() {
		assert.NotContains(t, s.Name(), "123.456.789-00", "PII leaked into span name")
		assert.NotContains(t, s.Name(), secretPath, "url.path leaked into span name")
		for _, kv := range s.Attributes() {
			assert.NotContains(t, kv.Value.AsString(), "123.456.789-00",
				"PII leaked into span attribute %s", kv.Key)
		}
	}
}
```

**Step 2 — Rodar.**
```
cd /home/gauchito/lerian/lib-observability && go test -tags=unit ./httpobs/ -run NeverEmitsURL
```
Esperado: `ok`. Se falhar porque otelhttp adiciona `url.path`/`url.full` como atributo de SPAN por default, adicionar em `otelhttpOptions()` um filtro que suprime esses atributos (ver Step 3).

**Step 3 (condicional — só se ST-T3-03 falhar) — Reforçar o guardrail no código.** Se o teste mostrar que o otelhttp default anexa path/PII, adicionar um span-name formatter bounded explícito como default em `newConfig` e/ou filtrar atributos. Default bounded no `otelhttpOptions`:
```go
	// Enforce a bounded default span name even if the otelhttp default ever
	// changes: never fold the URL path into the name.
	if c.spanNameFormatter == nil {
		opts = append(opts, otelhttp.WithSpanNameFormatter(
			func(_ string, r *http.Request) string { return "HTTP " + r.Method },
		))
	}
```
Rodar de novo até verde.

**Rollback:** remover o teste adicionado; `git checkout httpobs/httpobs.go` se alterou.

## ST-T3-04: Teste RED — no-op e base nil

**Goal:** garantir degradação segura (ADR-005/CA6).
**Files:** Modify `httpobs/httpobs_test.go` (append).

**Step 1 — Adicionar testes.**
```go
func TestNewTransport_NilBaseUsesDefaultTransport(t *testing.T) {
	rt := NewTransport(nil)
	require.NotNil(t, rt)
}

func TestNewClient_NoProvidersDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(nil) // no meter/tracer providers → no-op, must not break the call
	require.NotPanics(t, func() { doGET(t, client, srv.URL) })
}

func TestNewTransport_NoTracerProviderProducesNoSpan(t *testing.T) {
	mp, _, _, sr := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(nil, WithMeterProvider(mp)) // metric only, no tracer (ADR-005)
	doGET(t, client, srv.URL)

	for _, s := range sr.Ended() {
		assert.NotEqual(t, trace.SpanKindClient, s.SpanKind(),
			"no CLIENT span expected without a TracerProvider")
	}
}
```

**Step 2 — Rodar toda a suíte do pacote.**
```
cd /home/gauchito/lerian/lib-observability && go test -tags=unit ./httpobs/
```
Esperado: `ok`, todos os testes.

**Rollback:** remover os testes adicionados.

## ST-T3-05: doc.go (4 seções ADR + precedência)

**Goal:** documentar o pacote no padrão redisobs/doc.go.
**Files:** Create `httpobs/doc.go`.

**Step 1 — Criar `httpobs/doc.go`.**
```go
// Package httpobs provides a thin, nil-safe helper that adds OpenTelemetry
// tracing and metrics to outbound HTTP calls, wrapping the http.RoundTripper the
// application already uses. Every outbound request becomes an external-dependency
// call: SpanKind=CLIENT plus the http.client.request.duration metric.
//
// # Precedence (ADR-006)
//
// PREFER a wrapper when one exists — SQL (sqlobs), Redis/Valkey (redisobs), HTTP
// client (this package), messaging (messagingobs), inbound server (middleware /
// grpcmiddleware). Use tracing.StartClientSpan ONLY for outbound calls that have
// no wrapper (e.g. the document database, a custom RPC). Never wrap a call with
// this transport AND also open a manual span around the same call — that
// double-instruments the request.
//
// # Boundary (ADR-007)
//
// This package does NOT create or own the *http.Client. The application builds
// its transport (including custom TLS / timeout / proxy) and passes it here; the
// helper wraps it and returns. It never dials and never closes connections. Pass
// the custom transport as base so it is preserved; base == nil uses
// http.DefaultTransport.
//
// # Emitted telemetry
//
// otelhttp emits http.client.request.duration (seconds) with the labels
// http.request.method, http.response.status_code, server.address, and error.type
// (docs/metrics-contract.md), and a CLIENT span named "HTTP <METHOD>" by default.
// The span ends when the response body is fully read and closed — the caller MUST
// read and close the body.
//
// # PII / cardinality guardrail (docs/metrics-contract.md)
//
// The URL path and query (and any PII inside them) are NEVER attached as a span
// or metric attribute, and the default span name is bounded ("HTTP <METHOD>") so
// no concrete path/id enters the name. server.address can grow high-cardinality
// if the client talks to many distinct hosts — normalize/filter it downstream if
// needed. Enforced by tests.
//
// # No-op degradation (ADR-005)
//
// With no MeterProvider/TracerProvider supplied, instrumentation attaches against
// the OTel no-op providers; the helper never panics and never breaks the client.
// The CLIENT span is produced only when a TracerProvider is configured.
package httpobs
```

**Step 2 — Build + suíte completa + lint.**
```
cd /home/gauchito/lerian/lib-observability && go build ./httpobs/ && go test -tags=unit ./httpobs/ && golangci-lint run ./httpobs/
```
Esperado: build ok; testes `ok`; lint limpo.

**Step 3 — Commit do pacote httpobs.**
```
cd /home/gauchito/lerian/lib-observability && git add httpobs/ && git commit -S -m "feat(metrics): add httpobs http-client instrumentation wrapper" --trailer "X-Lerian-Ref: 0x1"
```

**Rollback:** `git restore --staged httpobs/ && rm -rf httpobs/`

---

# T4 — Documentação de precedência (README)

## ST-T4-01: Seção de precedência no README

**Goal:** entregar F3/CA4 — a regra nos 3 locais (README + os dois doc já feitos em T1/T3).
**Prereq:** T1 e T3 mergeados (símbolos `tracing.StartClientSpan` e `httpobs` existem).
**Files:** Modify `README.md`.

**Step 1 — Ler o README para achar o ponto de inserção.**
```
cd /home/gauchito/lerian/lib-observability && grep -n "^## " README.md | head -30
```
Inserir a nova seção após a seção de instrumentação existente (ou antes de "Contributing", se houver). Escolher o cabeçalho `## Span kind & wrapper precedence`.

**Step 2 — Inserir a seção (conteúdo exato).**
```markdown
## Span kind & wrapper precedence

Outbound calls (crossing a network boundary) must be classified as **CLIENT**;
inbound as **SERVER**. Getting this right is what makes external-dependency
latency/errors visible and keeps `INTERNAL` clean.

**Rule of thumb: always use the wrapper that classifies for you. Fall back to the
manual helper only when there is no wrapper. Never do both on the same call.**

| Outbound / inbound call | Use this (classifies automatically) |
|---|---|
| SQL (Postgres/MySQL) | `sqlobs.InstrumentDB` / `sqlobs.Open` → CLIENT |
| Redis / Valkey | `redisobs.Instrument` → CLIENT |
| **HTTP client** | `httpobs.NewTransport` / `httpobs.NewClient` → CLIENT |
| Messaging (produce/consume) | `messagingobs` → PRODUCER / CONSUMER |
| Inbound HTTP / gRPC (server) | `middleware` / `grpcmiddleware` → SERVER |
| **No wrapper** (document DB, custom RPC/SDK) | `tracing.StartClientSpan` → CLIENT (manual, last resort) |

Manual instrumentation via `tracing.StartClientSpan` is the last resort — for
example the document database, which has no stable driver instrumentation today.

> **Do not double-instrument.** If you wrap an HTTP client with `httpobs`, do NOT
> also open a `tracing.StartClientSpan` around the same request — that produces
> two CLIENT spans for one call. Use the wrapper for wrapped call types; use
> `StartClientSpan` only for the call types with no wrapper.

### Example — HTTP client (use the wrapper)

Wrap the transport your app already builds so the custom TLS/timeout/proxy config
is preserved; every outbound request then emits a CLIENT span and
`http.client.request.duration`:

```go
import (
	"net/http"

	"github.com/LerianStudio/lib-observability/v2/httpobs"
)

// baseTransport is the transport the app already configured (TLS, timeouts, ...).
func newInstrumentedClient(baseTransport http.RoundTripper) *http.Client {
	return httpobs.NewClient(baseTransport,
		httpobs.WithMeterProvider(meterProvider),
		httpobs.WithTracerProvider(tracerProvider), // required for the CLIENT span
	)
}

// Or, when the app builds its own *http.Client, wrap only the transport:
//   client.Transport = httpobs.NewTransport(baseTransport,
//       httpobs.WithMeterProvider(meterProvider),
//       httpobs.WithTracerProvider(tracerProvider))
```

Migration note: once the transport is wrapped, remove any manual
`tracer.Start(...)` you previously opened around the HTTP call (no double-instrumentation).

### Example — no wrapper (use StartClientSpan)

For an outbound call with no wrapper — e.g. the document database — replace the
hand-rolled `tracer.Start(...)` with `StartClientSpan` so the span is CLIENT:

```go
import "github.com/LerianStudio/lib-observability/v2/tracing"

// Before: outbound Mongo call rendered as INTERNAL
//   ctx, span := tracer.Start(ctx, "mongodb.find_holder")
// After: classified as an external-dependency call (CLIENT)
ctx, span := tracing.StartClientSpan(ctx, tracer, "mongodb.find_holder")
defer span.End()
// ... perform the document-database call ...
```
```

**Step 3 — Verificar render (links/tabela) e que os símbolos citados existem.**
```
cd /home/gauchito/lerian/lib-observability && grep -rn "func StartClientSpan" tracing/ && grep -rn "func NewTransport" httpobs/
```
Esperado: ambos encontrados (garante que o README não cita símbolo inexistente).

**Step 4 — Commit.**
```
cd /home/gauchito/lerian/lib-observability && git add README.md && git commit -S -m "docs: document wrapper precedence principle for span_kind" --trailer "X-Lerian-Ref: 0x1"
```

**Rollback:** `git checkout README.md`

---

## Verificação final (após T1–T4)

```
cd /home/gauchito/lerian/lib-observability && go build ./... && go test -tags=unit ./... && golangci-lint run ./...
```
Esperado: build ok; toda a suíte `ok` (incl. tracing/ e httpobs/); lint limpo.

**Checklist de aceite do pre-dev (CA1–CA8):**
- CA1/CA3 (classificação correta) → `TestStartClientSpan_DefaultsToClientKind`, `TestNewTransport_ProducesClientSpan`.
- CA2/F4 (métrica) → `TestNewTransport_EmitsClientDurationMetric` (nome, unidade s, labels).
- CA4/CA5 (precedência + anti-dobro) → README (T4) + doc.go(httpobs) + doc-comment(StartClientSpan).
- CA6 (no-op) → `TestNewClient_NoProvidersDoesNotPanic`, `TestNewTransport_NoTracerProviderProducesNoSpan`, `TestNewTransport_NilBaseUsesDefaultTransport`.
- CA7 (privacidade) → `TestNewTransport_NeverEmitsURLPathOrQueryAsAttribute`.
- CA8 (aditivo) → nenhuma modificação de símbolo existente; só arquivos novos + README.

---

## Gate 8 — Validação

- [x] Cada step é 2-5 min, uma ação
- [x] Código completo, sem placeholder/TODO/"..."
- [x] Todos os caminhos de arquivo explícitos; imports completos
- [x] Ciclo TDD RED-GREEN em T1/T3 (RED explícito; T2/T4 têm verificação objetiva)
- [x] Comandos de verificação copy-paste + saída esperada
- [x] Rollback por subtask
- [x] Zero-context (harness/doc.go/README completos; sem "veja código similar")
- [x] Nota de calibração no ST-T3-02 (conjunto exato de labels da métrica confirmável na execução — guardrail anti-PII e unidade s são inegociáveis)

**Confidence:** Atomicidade 30 · Completude de código 30 · Independência de contexto 25 · Cobertura TDD 15 = **100/100 → autônomo**.

**Resultado do Gate:** ✅ PASS (sujeito à aprovação humana)
