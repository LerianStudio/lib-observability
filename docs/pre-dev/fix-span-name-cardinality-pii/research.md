# Gate 0 — Research: Fix span.name high cardinality + PII (Fiber middleware)

## Metadata
- **Date:** 2026-07-17
- **Feature:** fix-span-name-cardinality-pii
- **Repo:** lib-observability (Go 1.26.3, Fiber v2.52.13, OTel Go SDK v1.44.0, semconv v1.34.0)
- **Research mode:** modification (extending existing middleware)
- **Track:** Small (4 gates)
- **Agents dispatched:** repo-research-analyst (primary), best-practices-researcher, framework-docs-researcher

## Executive Summary
O middleware Fiber (`middleware/telemetry.go`) nomeia o span SERVER a partir do **path resolvido** (`c.Path()`, só com UUID mascarado) no momento da criação (linha 213/223), enquanto o `http.route` (template, baixa cardinalidade) só é aplicado como atributo **depois** do `c.Next()`. Isso causa (a) explosão de cardinalidade via spanmetrics connector, que usa `span.name` como dimensão default, e (b) vazamento de PII (chave Pix/CPF crua no nome). O fix canônico do OTel é renomear o span para `{method} {http.route}` após o routing. **Descoberta crítica:** o middleware `EndTracingSpans`, quando registrado antes das rotas (padrão do plugin consumidor), encerra o span ANTES do `applyTelemetrySpanAttributes` rodar → qualquer `SetName`/`SetAttributes` pós-`c.Next()` é silenciosamente descartado. O fix precisa lidar com essa ordem.

## Research Mode
**modification** — estamos alterando o comportamento de naming de span num middleware existente e testado. Foco primário em codebase (padrões, testes que travam comportamento, interação de componentes). Best practices e framework docs validaram a abordagem e revelaram caveats (sampling, fasthttp buffer, PII).

---

## Codebase Research (file:line)

### Ciclo de vida do span SERVER (`middleware/telemetry.go`, handler em `WithTelemetry`, começa :159)
| Evento | Linha | Detalhe |
|---|---|---|
| Nome do span computado | `:213` | `method + " " + replaceUUIDWithPlaceholder(c.Path())` — **path cru** (só UUID mascarado) |
| Span criado | `:223` | `tracer.Start(traceCtx, routePathWithMethod, trace.WithSpanKind(trace.SpanKindServer))` |
| `defer endState.End()` | `:226` | fim garantido do próprio WithTelemetry (idempotente via `sync.Once`, :78-93) |
| endState no contexto | `:230-231` | compartilhado — é como o EndTracingSpans acha o mesmo span |
| `c.Next()` | `:238` | handlers downstream rodam |
| status reconciliado | `:243` | `httpStatusCode(c, err)` |
| `applyTelemetrySpanAttributes` | `:245-254` | roda APÓS c.Next() |
| `http.route` setado (SetAttributes) | `:295-297`, `:311` | via `routeAttribute`; template correto |

### Helper de rota (`middleware/helpers.go`)
- `routeAttribute(c, effectiveStatus) (string,bool)` — `:50-65`. Template de `c.Route().Path` (`:64`). Fallback 404: omite se `status==404 && r.Path=="/" && c.Path()!="/"` (`:60-62`). **Correto por spec OTel.**
- `replaceUUIDWithPlaceholder` — `:222-225`; regex `:16-17` só casa UUID canônico 8-4-4-4-12. **NÃO mascara IDs numéricos (`06881656483`), slugs, CPF/CNPJ.** Origem do vazamento no nome.

### ⚠️ Interação EndTracingSpans (o ponto arquitetural crítico)
- `EndTracingSpans` — `middleware/telemetry.go:399-422`; encerra span via `state.End()` (`:413`). É middleware **separado**, registrado independentemente pelo consumidor (sem wiring no repo).
- **Hazard confirmado:** se registrado `WithTelemetry` (outer) → `EndTracingSpans` (inner, antes das rotas), o unwind LIFO faz `EndTracingSpans.state.End()` (:413) rodar ANTES do `WithTelemetry.applyTelemetrySpanAttributes` (:245). Span OTel é imutável após `End()` → `SetName`/`SetAttributes`/`SetStatus` viram **no-op silencioso**. Hoje isso já faz o plugin perder `http.route`, `status_code`, `error.type` nas rotas registradas após o EndTracingSpans. gRPC análogo: `EndTracingSpansInterceptor` :506-523.

### Métrica nativa (modelo do fix — já correto)
- `http.server.request.duration` — const `:31`, record `recordHTTPServerDuration` `:350-381` (record `:380`). Usa `http.route` template via `routeAttribute` (`:367-369`), NÃO span.name. Docstring `:330-332` confirma. **O lado métrica já está certo; o problema é só o span name.**

### Testes que travam o comportamento atual (precisam mudar)
- `telemetry_test.go:172-187` (`TestWithTelemetry`): span name = `method + " " + expectedPath` (path cru UUID-masked). **Trava naming cru.**
- `telemetry_test.go:284-292` (`TestWithTelemetryExcludedRoutes`): `expectedSpanName := method + " " + replaceUUIDWithPlaceholder(path)`. **Trava naming cru.**
- `telemetry_test.go:378-386`, `:435-438`: semântica de End do EndTracingSpans.
- `telemetry_route_test.go:41`: span criado mesmo em 404.
- `telemetry_metrics_test.go:166-168`: `http.route=="/api/users/:id"` na métrica ("must use route template, never raw path") — **é o modelo do fix**.
- `telemetry_metrics_test.go:679-686`, `:704-711`: http.route ausente em 404 / `"/"` em root real.
- Faltam testes unitários isolados de `routeAttribute` e `replaceUUIDWithPlaceholder` — adicionar.

### Sem hook público de naming
Não existe `SpanNameFormatter`/`WithSpanNameFormatter`. Naming 100% interno (:213/:223). Consumidor não tem override. Único knob: `excludedRoutes ...string` (:132). Porém o formato do span name é um **contrato comportamental observável** (dashboards/Tempo), relevante p/ versionamento.

### docs/solutions/
**Ausente.** Prior art = CHANGELOG v1.1.0 (:5-25): já houve trabalho de baixa cardinalidade no lado métrica ("Omit http.route for unmatched 404", "Normalize http.request.method"). O fix de span name é o próximo passo lógico, mesma abordagem semconv-driven.

### Convenções (CLAUDE.md:26-41)
- Commit: `git commit -S -m "type(scope): message" --trailer "X-Lerian-Ref: 0x1"` — **GPG-signed obrigatório** + trailer.
- Conventional commits (feat/fix/chore/docs/refactor/test). Este = `fix(middleware): ...`.
- CI: `golangci-lint run ./...`; `go test -tags=unit ./...` (testes carregam `//go:build unit`); mocks via mockgen commitados.
- Release: semantic-release. **develop → beta**, **main → stable**. Mudança de formato do span name é behavior change (semver-relevante).

---

## Best Practices Research (URLs)

- **Span name HTTP server = `{method} {http.route}` (template)** — https://opentelemetry.io/docs/specs/semconv/http/http-spans/ — spec estável (2023). Exemplo canônico `GET /webshop/articles/:article_id`. "Instrumentation MUST NOT default to using URI path as `{target}`" — path resolvido é **proibido**.
- **http.route omitido em unmatched/404** — mesma spec. Fallback correto: nome degrada p/ só `{method}`, sem http.route. Confirma o helper atual.
- **`span.SetName()` pós-start/pré-End é suportado** (spec `UpdateName`) — https://opentelemetry.io/docs/specs/otel/trace/api/. **Caveat:** decisão de sampling foi tomada na criação com o nome original e NÃO é reavaliada no rename.
- **Nunca embutir ID/path/PII no nome** — https://opentelemetry.io/blog/2025/how-to-name-your-spans/ — IDs vão em atributos, não no nome.
- **PII em telemetria** — https://opentelemetry.io/docs/security/handling-sensitive-data/ — minimização; scrubbing de url.path; hash de CPF é reversível (espaço pequeno) → preferir drop/truncate. Usar template no nome exclui PII estruturalmente.
- **spanmetrics usa span.name como dimensão default** — https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/connector/spanmetricsconnector/README.md — template no nome = cardinalidade limitada. Failure mode documentado: `GET /product/1YMWWN1N4O`. Safety-net collector `set_semconv_span_name()` (complementar, não substituto).

---

## Framework Documentation (versões)

- **Fiber v2.52.13** — `c.Route().Path` = template; **confiável só APÓS `c.Next()`** (doc avisa explicitamente). `c.Path()` = resolvido. Middleware `app.Use` deve ser registrado ANTES das rotas que envolve; unwind LIFO (código após `c.Next()` = fase de resposta, rota já casada).
- **fasthttp v1.71.0** (indirect) — buffer recycling: strings de request (`c.Method()`, `c.OriginalURL()`...) precisam ser copiadas antes do `c.Next()` (já feito). **`c.Route().Path` NÃO precisa** (string do router, não do buffer) — safe pós-Next.
- **OTel Go v1.44.0** — `trace.Span.SetName(string)` existe e é válido pós-Start/pré-End (no-op se não-recording; guardar com `IsRecording()` opcional). Sem version skew entre otel/sdk/trace/metric.
- **semconv v1.34.0** — keys estáveis `http.route`, `http.request.method`, `http.response.status_code` (helpers `semconv.HTTPRoute(...)` etc.). Lib usa string literal idêntico hoje (drift médio: bump futuro não é pego pelo compilador; poderia migrar de brinde).

---

## Synthesis

### Padrões a seguir
- Modelar o fix no lado métrica já-correto: `recordHTTPServerDuration` usa `routeAttribute` pós-`c.Next()` (`telemetry.go:367-369`). Renomear o span no mesmo ponto.
- Nome = `{method} {http.route}` quando `routeAttribute` retorna `present`; fallback = nome provisório só-método (NÃO path cru) quando não casou rota.
- Migração opcional dos literais p/ `semconv.*` helpers enquanto edita.

### Constraints identificadas
1. **EndTracingSpans encerra o span antes do post-Next** (telemetry.go:413 vs :245) → `SetName`/`SetAttributes` pós-End são no-op. **O fix DEVE resolver isso**, não assumir. Opções: (a) aplicar nome/atributos antes de qualquer End compartilhado disparar; (b) coordenar o ciclo de vida do endState; (c) repensar a relação WithTelemetry/EndTracingSpans. **Acopla esta feature ao fix de ordem do plugin.**
2. **fasthttp buffer** — não introduzir leitura de request-string pós-Next sem cópia (c.Route().Path é exceção segura).
3. **Sampling** — rename tardio não reavalia sampling. Se houver sampler por nome, decisão fica no nome original. (Provavelmente não afeta head sampling atual, mas documentar.)
4. **Contrato comportamental** — mudança do formato do span name afeta dashboards/queries Tempo e é semver-relevante.
5. **Testes** — 2 testes travam o naming cru (telemetry_test.go:172-187, :284-292) → reescrever espelhando o teste da métrica.

### Prior solutions (docs/solutions/)
Ausente. CHANGELOG v1.1.0 mostra trabalho análogo de baixa cardinalidade no lado métrica — mesma filosofia.

### Open questions para o PRD/TRD
1. **Como resolver o hazard do EndTracingSpans?** Tornar a lib robusta à ordem (aplicar nome/attrs antes do End) OU depender do fix de ordem no plugin? (decisão de design central — afeta se a lib sozinha resolve ou precisa do plugin junto)
2. **Introduzir `SpanNameFormatter` opt-in** agora, ou só corrigir o naming internamente? (escopo)
3. **Migrar literais → semconv helpers** neste PR, ou manter escopo mínimo?
4. **É breaking change?** O formato do span name muda — precisa footer `BREAKING CHANGE`/bump major, ou é `fix` normal? (afeta consumidores com dashboards por span name)
5. **Fallback de nome em 404/unmatched:** só `{method}`, ou `{method} {path-uuid-masked}` como hoje? (spec prefere só método)
