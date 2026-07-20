# Tasks — Fix: span.name HTTP com alta cardinalidade e PII

## Metadata
- **Date:** 2026-07-17
- **Feature:** fix-span-name-cardinality-pii
- **Gate:** 3 (Task Breakdown — final do Small Track)
- **Inputs:** [research.md](./research.md), [prd.md](./prd.md), [trd.md](./trd.md)
- **Confidence:** 88/100 (design revisado por eng. Go; tasks pequenas, critérios testáveis; risco residual = contrato observável do nome)

## ⚠️ Alvo: Fiber v3 (branch `fix/span-name-cardinality-pii` a partir de `develop`)
A `develop` já migrou para **Fiber v3.4.0** (a `main` é v2.52.13). O research/TRD foi feito em v2, mas a estrutura do código em v3 é **idêntica** nos pontos do fix. Diferenças de API a usar (confirmadas no código da develop):
- `c.UserContext()` → `c.Context()`; `c.SetUserContext()` → `c.SetContext()` (telemetry.go:215/231).
- `EndTracingSpans(c fiber.Ctx)` / `routeAttribute(c fiber.Ctx, ...)` — `fiber.Ctx` é interface na v3 (sem ponteiro).
- `c.Route().Path`, guard 404 (helpers.go:60), `replaceUUIDWithPlaceholder`, `uuidPattern` — **inalterados**.
O núcleo do fix (span name :213/:223, applyTelemetrySpanAttributes :245, defer :226, EndTracingSpans :399-421) é conceitualmente igual ao TRD.

## Sequenciamento
```
T-001 (ownership do ciclo de vida) ──► T-002 (nomeação por template)
                                         │
                                         └─► ambas cobertas por T-003 (testes/robustez)
T-004 (semconv literais) — opcional, independente
```
T-001 é foundation (habilita o rename funcionar); T-002 entrega o valor (nome correto); T-003 é o DoD de robustez que valida os dois. Entregar como **um único PR** (mudança coesa e breaking), mas as tasks organizam a implementação e a verificação.

---

## T-001: Span raiz HTTP encerrado de forma determinística (ownership)
**Type:** Foundation

**Deliverable:** O span raiz criado pelo `WithTelemetry` é finalizado e encerrado por ele mesmo, exatamente uma vez, **independente da ordem** em que o consumidor registra `WithTelemetry`/`EndTracingSpans`.

**Scope:**
- Inclui: sinal de ownership no `spanEndState`; `WithTelemetry` como único encerrador do seu span raiz; `EndTracingSpans` pula spans owned; preservar fallback de span estrangeiro.
- Exclui: a nomeação em si (T-002); reclassificação de error.type (fora de escopo).

**Success Criteria:**
- **Funcional:** atributos pós-`c.Next()` (`http.route`, status, error.type) são aplicados ao span em **todas** as ordens de registro — hoje são perdidos quando `EndTracingSpans` está na cadeia.
- **Funcional:** span raiz encerrado exatamente uma vez em todas as 3 permutações posicionais e também **sem** `EndTracingSpans` registrado.
- **Funcional:** span estrangeiro/handler-criado continua encerrado pelo `EndTracingSpans` (fallback intacto — `TestEndTracingSpans_EndsFinalContextSpan` passa).
- **Técnico:** sem double-end, sem race (single-goroutine + flag; `sync.Once` mantido como defesa).
- **Qualidade:** 14 linters + `forbidigo` passam.

**User/Technical value:** habilita o rename de T-002 a funcionar de fato (sem isso, o rename seria descartado); e já corrige a perda silenciosa de `http.route`/status/error.type que existe hoje.

**Dependencies:** Blocks T-002. Requires: nenhum.

**Effort:** S (2-3 pts, ~1-2 dias).

**Risks:**
- *Mudança de comportamento do `EndTracingSpans` (deixa de encerrar o root do WithTelemetry).* Impacto: médio; Prob: média. Mitigação: no-op escopado só ao branch owned; fallback preservado; testes das 3 permutações. Fallback: reverter para encerramento coordenado (ADR-001 opção b).

**Testing:** unit (permutações de ordem, encerramento único, fallback estrangeiro).

**DoD:** revisado, testes passando, linters limpos, sem regressão em testes existentes de `EndTracingSpans`.

---

## T-002: Nome de operação HTTP = `{método} {rota}` (sem PII, baixa cardinalidade)
**Type:** Feature (entrega o valor principal do PRD — US-1, US-2, US-4)

**Deliverable:** Spans HTTP server são nomeados pelo template de rota (`GET /v1/dict/statistics/keys/:key`), nunca pelo caminho concreto com identificadores; nome livre de PII inclusive no momento da criação.

**Scope:**
- Inclui: nome na **criação** = só-método (PII-free para o sampler); rename para `{método} {rota}` após roteamento, guardado por `IsRecording()`; fallback só-método quando rota não reconhecida (reusa guard 404 existente).
- Exclui: hook público de formatação (não incluído — ADR-004); mudança na métrica de duração (já correta).

**Success Criteria:**
- **Funcional:** requisições ao mesmo endpoint com identificadores diferentes compartilham um único nome de span.
- **Funcional/Privacidade:** nenhum identificador concreto (CPF/chave Pix/numérico/slug) aparece no nome — nem no nome de criação, nem no final, nem em rotas não casadas.
- **Funcional:** `http.route` permanece disponível como atributo (US-3); nome e métrica de duração concordam sobre a rota (US-4).
- **Funcional:** rota não reconhecida (404/catch-all) → nome só-método, `http.route` omitido.
- **Qualidade:** convenção OTel `{method} {http.route}` respeitada.

**User/Technical value:** elimina a explosão de cardinalidade e o vazamento de PII na origem, para todos os consumidores da lib.

**Dependencies:** Requires T-001 (senão o rename é descartado).

**Effort:** S (3 pts, ~1-2 dias).

**Risks:**
- *Breaking change do formato do nome* (dashboards/queries por span name antigo). Impacto: médio-alto; Prob: alta. Mitigação: nota de release BREAKING + comunicação a donos de dashboard; versionamento (beta antes de stable). Fallback: N/A (é o comportamento desejado).
- *Sampling por nome de span* usa nome de criação (agora só-método). Mitigação: documentar na release; nome só-método já é PII-free.

**Testing:** unit (nome por template; nome PII-free na criação; fallback 404; consistência nome↔`http.route`).

**DoD:** revisado, testes passando, nota BREAKING preparada, linters limpos.

---

## T-003: Cobertura de testes e robustez (DoD transversal de T-001+T-002)
**Type:** Foundation (garantia de não-regressão)

**Deliverable:** Suíte de testes que trava o novo comportamento e previne regressão da cardinalidade/PII e do ciclo de vida do span.

**Scope:**
- Inclui: atualizar testes que travam o nome antigo; testes de robustez (3 permutações + sem EndTracingSpans + span estrangeiro); testes unitários novos de `routeAttribute` e do mascaramento de identificadores; assert de nome PII-free na criação.
- Exclui: testes de integração fora do escopo unit.

**Success Criteria:**
- **Técnico:** testes que asseravam o nome cru (path) atualizados para asserir o nome por template.
- **Técnico:** existem testes cobrindo as 3 permutações posicionais + "sem EndTracingSpans" + span estrangeiro.
- **Técnico:** `routeAttribute` e o mascaramento têm testes unitários isolados (hoje inexistentes).
- **Qualidade:** tag `//go:build unit` mantida; mocks via mockgen; `go test -tags=unit ./...` verde; cobertura conforme padrão do repo.

**User/Technical value:** garante que o fix não regrida e documenta o contrato novo via testes.

**Dependencies:** Requires T-001, T-002.

**Effort:** S-M (3-5 pts, ~2 dias).

**Risks:**
- *Testes existentes acoplados ao comportamento antigo além dos 2 mapeados.* Mitigação: rodar a suíte completa; ajustar os que quebrarem por dependerem do nome cru.

**Testing:** é a própria task.

**DoD:** suíte verde, cobertura ok, linters limpos.

---

## T-004 (opcional): Alinhar literais de atributo aos helpers canônicos
**Type:** Polish

**Deliverable:** Atributos HTTP usam os helpers canônicos de convenção em vez de strings literais, se trivial e baixo risco.

**Scope:** inclui só a troca literal→helper nos atributos já emitidos. Exclui qualquer mudança de valor/semântica; exclui reclassificação de error.type (proibida neste fix).

**Success Criteria:** valores de atributo idênticos aos atuais (sem mudança observável); compila e testes passam.

**User/Technical value:** remove drift latente (bump futuro de convenção pego pelo compilador).

**Dependencies:** Optional; independente de T-001/T-002.

**Effort:** S (1 pt). **Só fazer se não aumentar risco/escopo do PR.**

**Risks:** baixo. Mitigação: se qualquer valor divergir, não incluir.

---

## Release (aplicável a todas as tasks — um PR coeso)
- Commit **GPG-signed** + trailer `X-Lerian-Ref`; conventional commit `fix(middleware): ...`.
- **Footer `BREAKING CHANGE`** (formato do span name muda; dashboards por span name antigo afetados).
- Target **`develop`** (beta) → validar → **`main`** (stable). Semantic-release cuida do versionamento.
- Nota de release comunicando: mudança de nome de span (método+rota), impacto em dashboards/queries por nome antigo, e o caveat de sampling por nome.

## Gate 3 — Validação
| Categoria | Status |
|---|---|
| Todos os componentes do TRD cobertos | ✅ (ADR-001→T-001, ADR-002→T-002, testes→T-003, ADR-004→T-004) |
| Cada task entrega incremento funcional | ✅ |
| Critérios de sucesso testáveis | ✅ |
| Dependências mapeadas | ✅ |
| Nenhuma task > 2 semanas | ✅ (todas S/S-M) |
| Estratégia de teste definida | ✅ |
| Riscos com mitigação | ✅ |

**Resultado:** ✅ PASS — pre-dev Small Track COMPLETO. Pronto para implementação (dev-cycle).
