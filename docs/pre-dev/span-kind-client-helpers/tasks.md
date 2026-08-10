# Gate 7 — Task Breakdown: span-kind-client-helpers

> Gates anteriores: prd · feature-map · trd · api-design · data-model · dependency-map (mesmo dir)
> Data: 2026-07-27 · Track: Full · Status: draft (aguardando aprovação)

## Sequência de entrega

```
T1 (StartClientSpan, sem dep) ──┐
                                 ├──► T4 (docs precedência, referencia T1+T3)
T2 (dep otelhttp) ──► T3 (httpobs) ─┘
```
- **T1 é independente** (sem dep nova) → pode shipar sozinho/primeiro. Caminho crítico p/ o valor HTTP: T2→T3.
- **Paralelizável:** T1 e T2 podem começar juntos. T4 fecha (referencia T1 e T3).
- Cada task = 1 PR, commitável, testado, verde no CI.

---

## T1 — Classificação manual de saída como CLIENT (`tracing.StartClientSpan`)

- **Tipo:** Feature (Foundation p/ casos sem wrapper)
- **Deliverable:** um helper público `tracing.StartClientSpan` que inicia um span já classificado CLIENT sobre o tracer existente; demoável = um teste mostra o span saindo como CLIENT por default e respeitando override do caller.
- **Scope:**
  - **Inclui:** novo arquivo `tracing/spanhelpers.go` com `StartClientSpan(ctx, tracer, name, opts...)` (prepend `WithSpanKind(SpanKindClient)`, ADR-004); doc-comment com aviso de precedência inline (C3) + exemplos de uso legítimo (banco de documentos, RPC custom).
  - **Exclui:** `StartServerSpan`/`StartInternalSpan` (decisão: não expor); httpobs (T3); README (T4).
- **Success Criteria (testável):**
  - Funcional: chamar `StartClientSpan` produz span com kind = CLIENT quando o caller não passa kind.
  - Funcional (override, ADR-004): se o caller passa `WithSpanKind(...)` explícito, o do caller prevalece (last-wins via prepend).
  - Técnico: nil-safe conforme semântica do tracer da lib (não entra em panic com tracer resolvido pela lib).
  - Boundary: NÃO emite métrica; NÃO valida name/attrs.
  - Qualidade: `golangci-lint run ./...` limpo; `go test -tags=unit ./...` verde; cobertura da função ≥ padrão do repo.
- **User value:** engenheiro instrumenta corretamente uma saída SEM wrapper (ex.: banco de documentos) sem conhecer detalhes de baixo nível.
- **Technical value:** primeiro produtor de `SpanKindClient` da lib; caminho oficial p/ Mongo (ADR-003).
- **Componentes:** C2 (TRD). Sem dep nova (usa `otel/trace` já presente).
- **Dependencies:** Requires: nenhuma. Blocks: T4 (docs referenciam). Optional: —
- **Effort:** **S** (1-3 pts, ~1 dia).
- **Riscos:**
  - Append em vez de prepend → forçaria CLIENT e quebraria override. *Mitigação:* teste explícito de override; ADR-004 documentado. *Fallback:* corrigir ordem.
- **Testing Strategy:** unit (`tracing/spanhelpers_test.go`, build tag `unit`, InMemoryExporter): kind default CLIENT; override respeitado; nil-safety. Table-driven.
- **PR/commit:** `feat(tracing): add StartClientSpan helper` — GPG `-S` + trailer `X-Lerian-Ref: 0x1`.
- **DoD:** code review; unit verde; lint limpo; doc-comment com precedência; sem breaking; PR scope na allowlist.

---

## T2 — Dependência de instrumentação HTTP-cliente disponível (`otelhttp v0.69.0`)

- **Tipo:** Foundation
- **Deliverable:** `go.mod`/`go.sum` com `otelhttp v0.69.0` disponível para importar; demoável = build compila importando o pacote; `go mod tidy` estável.
- **Scope:**
  - **Inclui:** `go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.69.0` + `go mod tidy`; verificar que a ÚNICA transitiva nova é `felixge/httpsnoop v1.0.4`; otel core permanece v1.44.0.
  - **Exclui:** uso do pacote (T3).
- **Success Criteria (testável):**
  - `go build ./...` compila; `go mod tidy` não altera versões do otel core (permanece v1.44.0).
  - `go.sum` ganha `otelhttp v0.69.0` + `httpsnoop v1.0.4` e **nada além disso** de transitiva nova (verificar diff do go.mod/go.sum).
  - Sem bump de `go.opentelemetry.io/otel*`.
- **User value:** (habilitador) — sozinho não muda telemetria; destrava T3.
- **Technical value:** disponibiliza a instrumentação HTTP-cliente de mercado sem bump do otel core.
- **Componentes:** dep do C1 (dependency-map §2/§3).
- **Dependencies:** Requires: nenhuma. Blocks: T3. Optional: —
- **Effort:** **S** (1 pt, <½ dia).
- **Riscos:**
  - `go mod tidy` puxar transitiva inesperada / bumpar otel core. *Mitigação:* revisar o diff do go.mod/go.sum antes do commit; pin exato. *Fallback:* travar versões manualmente no require.
- **Testing Strategy:** build + `go mod verify`; inspeção do diff de go.mod/go.sum (esperado: só otelhttp + httpsnoop).
- **PR/commit:** `chore(deps): add otelhttp v0.69.0 for http client instrumentation` — commit ISOLADO (só go.mod/go.sum), GPG + trailer.
- **DoD:** build verde; diff de deps revisado e mínimo; licenças ok (Apache-2.0/MIT); commit isolado.

---

## T3 — Instrumentação automática de saída HTTP (pacote `httpobs`)

- **Tipo:** Feature (entrega o valor principal HTTP)
- **Deliverable:** pacote `httpobs` com `NewTransport(base, opts...)` e `NewClient(base, opts...)`; demoável = um teste mostra uma chamada HTTP de saída emitindo `http.client.request.duration` (unidade s, labels do contrato) + span CLIENT, e nenhum dado de URL/PII virando label.
- **Scope:**
  - **Inclui:** `httpobs/httpobs.go` (`NewTransport`, `NewClient`, `config`, `newConfig`, Options: `WithMeterProvider`/`WithTracerProvider`/`WithPropagators`/`WithSpanNameFormatter`/`WithAttributes`); `httpobs/doc.go` (4 seções ADR + precedência); guardrails ADR-005 (span só com TracerProvider) e ADR-007 (labels literais, sem PII); default span-name limitado; `NewClient(base, opts...)` preserva TLS (API-design).
  - **Exclui:** StartClientSpan (T1); README (T4); gRPC-client (follow-up).
- **Success Criteria (testável):**
  - Funcional: chamada de saída via transporte instrumentado emite `http.client.request.duration`, **unidade `s`**, labels exatamente `{http.request.method, http.response.status_code, server.address, error.type}` (conforme metrics-contract.md:29).
  - Funcional: span de saída sai como **CLIENT** (quando TracerProvider injetado).
  - Guardrail (ADR-007/CA7): request a URL com id/PII no path/query → **NÃO** aparece como label da métrica nem attr do span (teste anti-vazamento, espelha sqlobs/redisobs).
  - No-op (ADR-005/CA6): sem providers → sem panic, sem efeito; sem TracerProvider → sem span (mas pode medir); `base==nil` → usa transporte padrão.
  - Conformidade: buckets = advisory HTTP (metrics-contract.md:21).
  - Qualidade: lint limpo; `go test -tags=unit ./...` verde.
- **User value:** SRE ganha latência/erro por dependência HTTP externa (identidade/Banco Central/tenants); engenheiro instrumenta saída HTTP trocando o transporte (1 linha).
- **Technical value:** primeiro produtor de `http.client.request.duration`; fecha o slot do contrato.
- **Componentes:** C1 (TRD); dep otelhttp (T2).
- **Dependencies:** Requires: **T2**. Blocks: T4. Optional: T1 (independente, mas T4 referencia ambos).
- **Effort:** **M** (5-8 pts, ~3-4 dias).
- **Riscos:**
  - Cardinalidade de span-name se formatter custom injetar path. *Mitigação:* default limitado obrigatório + doc; teste anti-PII. *Fallback:* remover formatter custom.
  - Cardinalidade de `server.address` (muitos hosts). *Mitigação:* documentar risco + recomendação de View/filtragem no consumo. *Fallback:* orientar normalização.
  - Dupla-instrumentação (app usa httpobs E span manual). *Mitigação:* doc anti-dobro (ADR-006); T4.
  - `httpobs` não está na allowlist de scope. *Mitigação:* usar `feat(metrics)` (dependency-map §7). *Fallback:* expandir allowlist (follow-up).
- **Testing Strategy:** unit (`httpobs/httpobs_test.go`, tag `unit`, ManualReader + InMemoryExporter + httptest server): conformidade de métrica; span CLIENT; anti-PII; no-op; base nil. Sem mock novo (otelhttp é concreto; `httptest` cobre) — **não precisa mockgen**.
- **PR/commit:** `feat(metrics): add httpobs http-client instrumentation wrapper` — GPG + trailer. (Após T2 mergeado.)
- **DoD:** code review; unit verde (incl. anti-PII e conformidade de contrato); lint limpo; doc.go completo (4 seções + precedência); sem breaking; PR scope válido.

---

## T4 — Princípio de precedência documentado (README + convenção)

- **Tipo:** Feature (entrega F3/CA4/CA5 — sua ênfase; é primeira classe, não polish)
- **Deliverable:** documentação que torna o princípio de precedência encontrável e inequívoco; demoável = revisão confirma a regra nos locais exigidos + exemplo + alerta anti-dobro.
- **Scope:**
  - **Inclui:** seção no README da lib com **tabela de precedência** (por tipo de saída → wrapper: SQL→sqlobs, cache→redisobs, HTTP→httpobs, mensageria→messagingobs, servidor→middleware; "manual `StartClientSpan` só sem wrapper"; alerta anti-dobro) **+ exemplos de código de uso/adoção** (copy-paste): (a) `httpobs.NewClient`/`NewTransport` envolvendo o transport TLS da app; (b) `tracing.StartClientSpan` substituindo `tracer.Start("mongodb.*")`, com nota de migração (remover span manual ao adotar wrapper). Consistência com os doc.go de T1 (StartClientSpan) e T3 (httpobs) já escritos nessas tasks.
  - **Exclui:** doc.go de httpobs (feito em T3) e doc-comment de StartClientSpan (feito em T1) — T4 garante o README e a consistência entre os três.
- **Success Criteria (testável por revisão — CA4/CA5):**
  - README contém a tabela de precedência + regra "automático quando há wrapper; manual só sem wrapper".
  - README + doc.go(httpobs) + doc-comment(StartClientSpan) contêm o alerta anti-instrumentação-em-dobro.
  - Cada um traz ao menos um exemplo de quando o caminho manual é legítimo (banco de documentos).
  - README traz **exemplos de código copy-paste**: httpobs envolvendo transport TLS (com WithTracerProvider) + StartClientSpan substituindo tracer.Start("mongodb.*") com nota de migração.
  - Legível para humano E IA (linguagem direta, sem ambiguidade).
- **User value:** qualquer leitor (humano/IA) aplica o padrão certo desde a primeira leitura; padrão não degrada.
- **Technical value:** trava a convenção que corrige a causa-raiz (falta de padrão claro → 1.052 vs 111).
- **Componentes:** C3 (TRD).
- **Dependencies:** Requires: **T1, T3** (referencia os símbolos). Blocks: —. Optional: —
- **Effort:** **S** (1-2 pts, ~½-1 dia).
- **Riscos:**
  - Regra existir só num lugar. *Mitigação:* CA4 exige os 3 locais; checklist de revisão. *Fallback:* revisão de docs bloqueia merge.
- **Testing Strategy:** revisão de documentação (checklist CA4/CA5); sem teste automatizado (conteúdo).
- **PR/commit:** `docs: document wrapper precedence principle for span_kind` — GPG + trailer.
- **DoD:** revisão aprovada (3 locais + exemplo + anti-dobro); links corretos; consistente com doc.go/doc-comment.

---

## Follow-ups (FORA de escopo — registrados)

| Item | Por quê fica fora | Onde retomar |
|---|---|---|
| **gRPC-client span CLIENT** | `grpcmiddleware.UnaryClientInterceptor` mede mas não cria span CLIENT (gap real, análogo ao HTTP) | Nova feature/pre-dev; mesma raiz |
| **Migração das apps** (midaz/plugins adotarem httpobs/StartClientSpan; remover spans manuais) | Esta feature entrega os MEIOS; adoção é downstream | Trabalho por app |
| **Expandir allowlist de scope p/ `httpobs`** (scopes por-pacote-obs) | Decisão de convenção de CI mais ampla; hoje usa `feat(metrics)` | Se o time quiser padronizar scopes por-pacote (migra sqlobs/redisobs/messagingobs junto) |
| **Cobertura automática de Mongo** | otelmongo v2 instável (ADR-003); hoje via StartClientSpan manual | Reavaliar quando otelmongo v2 amadurecer |
| **Redigir `url.full` no OTel Collector** (transform/redact) | otelhttp sempre grava url.full no span; OTel-Go não tem hook p/ remover (ADR-008). Métrica já limpa; falta o trace | Repo de config do collector (NÃO a lib). Usuário indicou que transform provavelmente já existe — validar cobertura de url.full |
| **Auditoria de adoção do `messagingobs`** | prod mede só 4 CONSUMER, 0 PRODUCER; consumer.*/event.*/reconcile caem em INTERNAL. Wrapper JÁ existe — gap é de adoção, mesma raiz do CLIENT | Varredura nas apps + migração p/ NewPublisher/NewConsumer |
| **Regra de span_kind em agent do Ring pre-dev** | detectar tracer.Start cru sem WithSpanKind (recomendar wrapper/StartClientSpan) NO planejamento, não como linter de CI | Extensão do Ring dev-sre/checklist o11y (ver memória project-span-kind-lint-ring-agent) |

---

## Gate 7 — Validação

- [x] Todos os componentes do TRD cobertos (C1→T3, C2→T1, C3→T4; dep→T2)
- [x] Todas as features do PRD cobertas (F1→T3, F2→T1, F3→T4, F4→T3, F5→T1/T3)
- [x] Cada task entrega software funcionando e demoável (T2 é habilitador com critério objetivo)
- [x] Success criteria mensurável/testável por task
- [x] Dependências corretas (T1 indep.; T2→T3; T1+T3→T4)
- [x] Nenhuma task > 2 semanas (maior = T3, M ~3-4 dias)
- [x] Estratégia de teste por task; riscos com mitigação/fallback
- [x] Scope de PR por task (allowlist) + GPG/trailer
- [x] Follow-ups fora de escopo registrados

**Confidence:** Decomposição 30 · Clareza de valor 25 · Dependências 25 · Estimativa 20 (baseada em sqlobs/redisobs precedentes) = **100/100 → autônomo**.

**Resultado do Gate:** ✅ PASS (sujeito à aprovação humana)
