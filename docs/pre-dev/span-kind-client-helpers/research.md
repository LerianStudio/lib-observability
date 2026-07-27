# Gate 0 — Research: span-kind-client-helpers

> Track: Full (9 gates) · Modo: **modification** · Data: 2026-07-26
> Agentes: repo-research-analyst · best-practices-researcher · framework-docs-researcher
> Repo: `github.com/LerianStudio/lib-observability/v2` (branch `develop`, Go 1.26.3, otel v1.44.0)

---

## Executive Summary

A feature adiciona 2 helpers para instrumentar chamadas de SAÍDA como `span_kind=CLIENT` (hoje caem em `INTERNAL` por default): **(1)** `StartClientSpan` em `tracing/spanhelpers.go`; **(2)** pacote novo `httpobs/` com `NewTransport`/`NewClient` sobre `otelhttp`. A pesquisa confirmou precedente interno forte (sqlobs/redisobs = Option pattern + sentinel error + guardrail PII + doc.go/ADR), validou por código-fonte OTel que **PREPEND** de `WithSpanKind` é o correto (options são last-wins), e **corrigiu 3 suposições do briefing**: (a) versão do otelhttp é **v0.69.0** (não v0.59.x); (b) labels HTTP na lib são **strings literais**, não `constants/` (que só tem `db.*`); (c) o span-name default do otelhttp já é bounded (`"HTTP GET"`), o risco de cardinalidade só surge com formatter custom.

## Research Mode

**modification** — estende uma lib existente com padrões já estabelecidos (sqlobs/redisobs/messagingobs). Foco primário = codebase research (padrões a imitar, file:line). Confirmado: a feature é **aditiva pura** — `SpanKindClient` e um produtor de `http.client.request.duration` **não existem hoje** (zero colisão com código existente).

---

## Codebase Research (repo-research-analyst)

### Pattern 1 — Option pattern canônico (config + Option + WithX + newConfig)
- **sqlobs:** `sqlobs/sqlobs.go` — `config` struct **:50-57**; `type Option func(*config)` **:60**; `WithMeterProvider` **:65-71**; `WithTracerProvider` **:75-81**; `WithAttributes` **:109-113**; `newConfig` (defaults `otel.GetMeterProvider()/GetTracerProvider()`) **:115-126**.
- **redisobs:** `redisobs/redisobs.go` — `config` **:19-23**; `Option` **:26**; `WithMeterProvider` **:30-36**; `WithTracerProvider` **:40-46**; `WithAttributes` **:51-55**; `newConfig` **:57-68**.
- Nil-guard dentro de cada `WithX` (`if mp != nil { c.meterProvider = mp }`, sqlobs:66-69 / redisobs:31-35) — Options nunca sobrescrevem com nil.
- → **httpobs replica EXATAMENTE.** Options a expor: `WithMeterProvider`, `WithTracerProvider`, `WithPropagator(s)`, `WithSpanNameFormatter`, `WithAttributes`.

### Pattern 2 — Sentinel error + nil-safe
- `ErrNilDB` (`sqlobs/sqlobs.go:46-48`), usado **:207-209**, **:248-250**. `ErrNilClient` (`redisobs/redisobs.go:15-16`), usado **:81-83**. Formato: `var Err... = errors.New("pkg: msg")` no topo + early guard.
- → httpobs: `base==nil` **NÃO é erro** (cai em `http.DefaultTransport`). Sentinel só se `NewClient`/config puder falhar.

### Pattern 3 — Guardrail PII/cardinalidade (aplicado incondicional + documentado)
- sqlobs: `DisableQuery: true` em `otelsqlOptions` **:153-162** (comentário cita ADR-002 §3 + metrics-contract.md :154). Disclaimer em `WithAttributes` **:106-108**.
- redisobs: `redisotel.WithDBStatement(false)` **:97** (comentário cita ADR-004 :94-96). Disclaimer **:48-50**.
- → httpobs: **`url.path`/`url.query` NUNCA como label** (só `server.address`/`http.request.method`/`http.response.status_code`/`error.type`). Comentário padrão `// GUARDRAIL (ADR-00X, docs/metrics-contract.md): ...`.

### Pattern 4 — doc.go com 4 seções ADR-referenciadas
- `redisobs/doc.go:1-30`, `sqlobs/doc.go:1-39`. Estrutura fixa: preâmbulo → `# Boundary (ADR-00X)` → `# Emitted telemetry` → `# PII / cardinality guardrail (docs/metrics-contract.md)` (+"Enforced by tests") → `# No-op degradation (ADR-008)`.
- → `httpobs/doc.go` mesmo formato. Boundary = "não cria/possui o `*http.Client`, só envolve o RoundTripper".

### Pattern 5 — Spans com SpanKind explícito hoje + o GAP
- **Server (existem):** `middleware/telemetry.go:281` (`WithSpanKind(SpanKindServer)`); `grpcmiddleware/telemetry.go:263`.
- **Producer/Consumer:** `messagingobs/messagingobs.go:172` (Producer) / **:241** (Consumer). Mecânica de referência p/ `tracer.Start(ctx, name, WithSpanKind(...), ...)` = **:171-174**.
- **GAP gRPC client (follow-up, fora de escopo):** `grpcmiddleware/telemetry.go:392` (`UnaryClientInterceptor`) injeta ctx + mede `rpc.client.duration` mas **NÃO cria span CLIENT** (linhas 392-439).
- **`SpanKindClient` — INEXISTENTE:** grep no repo = ZERO ocorrências em código. `SpanKindInternal` só em `runtime/tracing_test.go:314` (teste). Confirma o problema (saída cai em INTERNAL default).

### Pattern 6 — NewTrackingFromContext devolve tracer cru (sem kind)
- `observability.go:130-142`: `func NewTrackingFromContext(ctx) (log.Logger, trace.Tracer, string, *metrics.MetricsFactory)` — devolve `trace.Tracer` cru, sem SpanKind. `StartClientSpan(ctx, tracer, name, ...)` é o wrapper drop-in.

### Contrato de métrica (`docs/metrics-contract.md`)
- **`http.client.request.duration`** (linha **29**): Histogram · unidade **s** · labels **`http.request.method, http.response.status_code, server.address, error.type`** · "STABLE (já existe)". **CONFIRMADO: nenhum produtor emite hoje** (grep) — só o server (`http.server.request.duration`, `middleware/telemetry.go:33`).
- Buckets HTTP advisory (linha 21): `0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10` — idênticos a `middleware/telemetry.go:43-47`. httpobs client usa os MESMOS.
- Unidade sempre `s` (princípio 1); instrumento criado 1× + record no-op se nil (princípio 2); PROIBIDO como label (linha 40-47): `url.path` com id/uuid, PII.

### Labels HTTP = strings literais (achado que corrige o briefing)
- Produtor server usa **strings literais**, não `constants/` nem semconv package: `middleware/telemetry.go:351` (`attribute.String("http.request.method", ...)`), **:354** (`attribute.String("server.address", ...)`). `server.address` já é ativado pelo middleware.
- `constants/opentelemetry.go` só tem chaves **`db.*`** — NÃO tem `http.*`. → httpobs usa strings literais (consistente com o server), NÃO inventa em constants.

### CI / PR conventions (`.github/workflows/pr-validation.yml:20-36`)
- Allowlist `pr_title_scopes`: `assert, ci, config, constants, core, deps, docs, log, metrics, middleware, redaction, runtime, scripts, tests, tracing, zap`. **`tracing` está** (StartClientSpan OK). **`httpobs` NÃO está** (CI rejeita `feat(httpobs):`). Sem scope por-pacote-obs (sqlobs/redisobs também não).
- Precedente: sqlobs/redisobs shipparam sob **`feat(metrics)`**. → httpobs sob `feat(metrics)` ou `feat(core)`; dep sob `deps`. Scope dedicado exigiria expandir allowlist no mesmo PR (edição `ci`).

### Testes-padrão (httpobs deve espelhar)
- Harness ManualReader + InMemoryExporter: `sqlobs/sqlobs_test.go:79-91`.
- Contrato de métrica (nome+unidade): `sqlobs_test.go:105-128` (filtra nome exato, `require.Equal(t,"s",m.Unit)` :119).
- Anti-PII: `sqlobs_test.go:206-262` (`TestInstrumentDB_NeverEmitsQueryText`), redisobs `:75-108`. → httpobs: request a URL com PII no path/query, asserir que não vira label/attr.
- Nil-safe / no-op: `sqlobs_test.go:294-315`. Build tag `//go:build unit` (linha 1).

### Prior work no repo (não há `docs/solutions/`)
- **observability-metrics-standardization** (`docs/pre-dev/`, NÃO committado, 2026-07-20): §4.3 (`api-design.md:60-65`) aposta em `WrapMongoOptions`+otelmongo v2. **NÃO tem StartClientSpan nem httpobs**; só gRPC client interceptor de métrica (§3.1, :30-32 = o GAP do Pattern 5). → fundamenta o ADR de divergência (Mongo) e a relação de COMPLEMENTO.
- **fix-span-name-cardinality-pii** (`docs/pre-dev/`, 2026-07-17): `research.md:54` "não existe SpanNameFormatter"; `prd.md:15-24` ~50k nomes distintos, PII no span.name. → REFORÇA default BOUNDED obrigatório no `WithSpanNameFormatter` do httpobs.

---

## Best Practices Research (best-practices-researcher)

### span_kind CLIENT vs INTERNAL (normativo)
- OTel Trace API spec: CLIENT = "request to a remote service where the client awaits a response" (cruza fronteira de rede); INTERNAL = default, in-process. → saída HOJE em INTERNAL está semanticamente errada.
- HTTP semconv: HTTP client **MUST** ser CLIENT. DB semconv: CLIENT quando out-of-process (caso comum), INTERNAL para in-memory. → documentar que `StartClientSpan` é para chamadas que cruzam rede.
- URLs: https://opentelemetry.io/docs/specs/otel/trace/api/#spankind · https://opentelemetry.io/docs/specs/semconv/http/http-spans/ · https://opentelemetry.io/docs/specs/semconv/db/database-spans/

### PREPEND vs APPEND (decisão de design, provada por fonte)
- `NewSpanStartConfig` aplica options em loop sequencial; `WithSpanKind` sobrescreve `cfg.spanKind` sem merge → **LAST-WINS**.
- → `StartClientSpan` deve **PREPEND** `WithSpanKind(SpanKindClient)`: `opts = append([]trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindClient)}, opts...)`. Assim o caller pode sobrepor com um `WithSpanKind` explícito (aparece depois, vence). APPEND forçaria CLIENT e ignoraria o caller silenciosamente.
- Fonte: https://github.com/open-telemetry/opentelemetry-go/blob/main/trace/config.go · https://pkg.go.dev/go.opentelemetry.io/otel/trace#NewSpanStartConfig

### otelhttp.NewTransport — uso recomendado
- Envolve um `http.RoundTripper` base; base nil → `http.DefaultTransport`. **Sempre passar o transport custom (TLS) como base** — swap silencioso dropa TLS/timeout/proxy (bug sutil).
- Default: span CLIENT + métrica `http.client.request.duration` + propagação W3C nos headers de saída. Span fecha ao body ser lido/fechado → **caller deve ler/fechar o body** senão vaza span.
- Default span name = `"HTTP <METHOD>"` (bounded, sem path). Risco só com formatter custom que injete `r.URL.Path`.

### Anti-patterns
1. **Dupla-instrumentação:** otelhttp.NewTransport + StartClientSpan manual na MESMA chamada = 2 spans CLIENT. Regra: **HTTP → otelhttp; StartClientSpan só p/ não-HTTP** (RPC/SDK custom sem lib de instrumentação, ex: Mongo).
2. **APPEND WithSpanKind:** viola last-wins, ignora override do caller.
3. **Formatter com path cru:** `"GET /users/{uuid}"` → span-name ilimitado.
4. **`server.address` ilimitado:** client que chama muitos hosts distintos explode séries → filtrar/normalizar via View; documentar como risco conhecido.
5. **Não fechar response body:** otelhttp fecha span no body close/EOF.

---

## Framework Documentation (framework-docs-researcher)

### Pin da nova dep (produto central desta pesquisa)
| Dependência | Pin | Requer otel core | Go min | Verificado via |
|---|---|---|---|---|
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | **v0.69.0** | `v1.44.0` (exato, não mais novo) | go 1.25.0 (ok, temos 1.26.3) | go.mod do pacote na tag |
| `github.com/felixge/httpsnoop` (transitiva, NOVA) | **v1.0.4** | — | — | go.mod do otelhttp na tag |

> **Correção do briefing:** a hipótese ~v0.59.x estava ERRADA. No monorepo opentelemetry-go-contrib, `net/http/otelhttp` e `instrumentation/runtime` compartilham o mesmo número v0.x no mesmo release train. `runtime v0.69.0` já está no go.mod → otelhttp é **v0.69.0** (verificado lendo o go.mod da tag: requer otel v1.44.0). `go mod tidy` adiciona só `felixge/httpsnoop v1.0.4` (indireta); otel core/metric/trace/sdk permanecem v1.44.0.

### O que otelhttp v0.69.0 emite por default
- Métrica **`http.client.request.duration`** · Histogram · unidade **`s`** · via helper estável `httpconv.NewClientRequestDuration` (semconv v1.41.0), buckets `0.005…10` = MATCH exato com metrics-contract.md:21/29. Labels: `http.request.method`, `http.response.status_code`, `server.address` (+ `server.port`, `network.protocol.*`), `error.type`.
- **MeterProvider** default = global `otel.GetMeterProvider()`. **TracerProvider** fica **nil** se não passado → **span CLIENT só é criado se `WithTracerProvider` for passado** (importante: caminho telemetry-on da lib DEVE passar TracerProvider).
- Propagação default = global `otel.GetTextMapPropagator()`.
- **Estabilidade de nome:** v0.69.0 emite o nome semconv estável em segundos por default; o legado `http.client.duration` (ms) só sob `OTEL_SEMCONV_STABILITY_OPT_IN=http/dup`. → v0.69.0 é o pin certo (sem ms, sem conversão no collector).

### API otelhttp a repassar (confirmada em v0.69.0)
`NewTransport(base http.RoundTripper, opts ...Option) *Transport` (implementa RoundTripper). Options presentes: `WithTracerProvider`, `WithMeterProvider`, `WithPropagators`, `WithSpanNameFormatter`, `WithServerName`, `WithSpanOptions`, `WithFilter`, etc. **Superfície mínima recomendada do httpobs:** `WithTracerProvider`, `WithMeterProvider`, `WithPropagators`, `WithSpanNameFormatter` (+ `WithAttributes` no estilo sqlobs). `WithMetricAttributesFn` é deprecated — não expor.

---

## Synthesis

### Padrões a seguir (com refs)
- **Option pattern / doc.go / testes:** clonar `redisobs/` (menor). Refs: redisobs.go:19-68, doc.go:1-30, redisobs_test.go.
- **Mecânica de span CLIENT:** `messagingobs/messagingobs.go:171-174` (tracer.Start + WithSpanKind).
- **Buckets + labels literais:** `middleware/telemetry.go:43-47` (buckets), :351-354 (labels string literais + server.address).
- **PREPEND WithSpanKind** (last-wins, provado): `trace/config.go`.
- **Pin otelhttp v0.69.0** (mesmo train do runtime já presente).

### Constraints identificados
- Layout flat no root (`httpobs/` no root, dir==pacote). CLAUDE.md.
- Commit GPG `-S` + trailer `X-Lerian-Ref: 0x1` + conventional.
- Scope de PR: `feat(tracing)` p/ StartClientSpan; `feat(metrics)`/`feat(core)` + `deps` p/ httpobs (não `httpobs`).
- Unidade `s`; instrumento 1×; no-op se nil; url.path/query nunca como label.
- Span CLIENT só sai se TracerProvider for passado ao otelhttp (não usar só globals no caminho on).
- Caller deve fechar response body (documentar).

### ADRs a registrar neste pre-dev
- **ADR-Mongo:** Mongo via `StartClientSpan` manual, NÃO otelmongo v2 (instável p/ mongo-driver/v2). Diverge de observability-metrics-standardization §4.3 (`WrapMongoOptions`). Decisão fechada pelo usuário 2026-07-25.
- **ADR-prepend:** `StartClientSpan` prepend (não append) — last-wins, caller pode sobrepor.
- **ADR-http-vs-manual:** HTTP → httpobs (otelhttp); StartClientSpan só p/ não-HTTP (Mongo/RPC custom). Evita dupla-instrumentação.
- **ADR-labels-literais:** labels HTTP como strings literais (espelha middleware), não constants (só tem db.*).

### Relação com pre-dev existente
COMPLEMENTA `observability-metrics-standardization` (frente RED) sem editá-lo: aquele plano LISTA `http.client.request.duration` como esperada mas não entrega o produtor; httpobs é o produtor. Único conflito = Mongo (resolvido acima).

### Open questions p/ o PRD
1. `StartClientSpan` deve expor também `StartInternalSpan`/`StartServerSpan` por simetria, ou só Client? (Producer/Consumer ficam em messagingobs.) → resolver no PRD/API-design.
2. httpobs expõe `NewClient` (conveniência) além de `NewTransport`? → sim (briefing), confirmar no API-design.
3. Scope de PR do httpobs: `feat(metrics)` (precedente) vs expandir allowlist p/ `httpobs`? → decisão no dependency-map/tasks.

### Blockers / negative findings
- `docs/solutions/` não existe (não é blocker; pre-devs cobrem).
- `git log` indisponível no ambiente do agente — precedente de scope inferido do código + briefing (confirmável com `git log --oneline -- sqlobs/ redisobs/`).
- `SpanKindClient` e produtor de `http.client.request.duration`: ambos inexistentes → feature aditiva pura, sem colisão.
