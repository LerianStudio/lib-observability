# TRD — Fix: span.name HTTP com alta cardinalidade e PII

## Metadata
- **Date:** 2026-07-17
- **Feature:** fix-span-name-cardinality-pii
- **Gate:** 2 (TRD, Small Track)
- **Inputs:** [research.md](./research.md), [prd.md](./prd.md)
- **deployment.model:** N/A (biblioteca compartilhada, não serviço)
- **tech_stack.primary:** Go (backend library)
- **tech_stack.standards_loaded:** golang/bootstrap.md (observability), golang/quality.md (linting/testing)
- **Confidence:** 90/100 (revisado por engenheiro Go; premissa do hazard corrigida, mecânica de ownership estabelecida, escopo do EndTracingSpans delimitado, nome PII-free na criação)
- **Go review:** ✅ Opção (a) validada como SÓLIDA com 4 correções aplicadas (premissa LIFO, flag owned vs sync.Once, nome PII-free na criação, no-op escopado a spans owned)

---

## 1. Análise / NFRs

| NFR | Alvo |
|---|---|
| **Cardinalidade** | Nome de operação HTTP em baixa cardinalidade (ordem de nº de rotas, não de requisições) |
| **Privacidade** | Zero identificador concreto (PII) no nome da operação, para qualquer formato de parâmetro |
| **Robustez** | Correção funciona **independente da ordem** em que o consumidor registra os middlewares de telemetria |
| **Compatibilidade** | Métrica de duração HTTP existente (que já usa o template de rota) **não** pode regredir; fallback 404 preservado |
| **Não-regressão de análise** | Rota permanece disponível como atributo para filtro/agrupamento |

### Mapeamento PRD → componentes
- US-1 (privacidade) + US-2 (cardinalidade) → **nomeação do span pelo template de rota** (componente: middleware HTTP de telemetria).
- US-3 (análise por rota) → **atributo de rota preservado** + **fallback seguro** para rota não reconhecida.
- US-4 (consistência) → nome do span e métrica de duração derivam da **mesma fonte de rota**.

---

## 2. Arquitetura da Solução

### Estilo
Modificação pontual de um **componente de middleware HTTP** dentro de uma biblioteca de observabilidade. Sem novo componente, sem nova fronteira de domínio. Padrão: alinhar o ciclo de vida de nomeação/finalização do span à convenção de indústria (método + template de rota).

### Componentes envolvidos
| Componente | Responsabilidade | Mudança |
|---|---|---|
| Middleware de telemetria HTTP (`WithTelemetry`) | Cria o span raiz da requisição, captura atributos, finaliza | Passa a **nomear o span pelo template de rota** e a **finalizar nome+atributos antes de qualquer encerramento** |
| Middleware de encerramento (`EndTracingSpans`) | Encerra o span raiz | Passa a ser **idempotente e seguro** — não pode "vencer a corrida" e encerrar o span antes da finalização |
| Helper de rota (`routeAttribute`) | Deriva o template + fallback 404 | **Reutilizado** como fonte única da rota (nome + atributo + métrica) |

### ADR-001 — Como resolver o hazard de ordem do middleware (o problema central)
> **Revisado após review de engenheiro Go (2026-07-17).** A premissa original ("só ocorre com ordem errada") estava incorreta — ver Context corrigido.

- **Context (CORRIGIDO):** O span raiz é criado por `WithTelemetry` (`telemetry.go:223`) mas o template de rota só é conhecido após o roteamento (`c.Next()`, `:238`), aplicado em `applyTelemetrySpanAttributes` (`:245`). O encerramento é feito hoje pelo middleware separado `EndTracingSpans` (`:399-422`, `state.End()` em `:413`), que compartilha o mesmo `endState` via contexto (`:230`). **Descoberta do review:** pelo LIFO do Fiber, o middleware registrado por ÚLTIMO é o mais interno e desenrola PRIMEIRO. Logo, com `EndTracingSpans` registrado **por último** (a ordem CORRETA do standard), ele ainda encerra o span **antes** do `WithTelemetry` rodar `applyTelemetrySpanAttributes`. **O hazard existe na ordem correta do standard, não só na errada.** Consequência: `http.route`, `status_code` e `error.type` já são perdidos hoje em qualquer consumidor que use os dois middlewares — é um defeito real, não só misconfiguração. (Spans OTel são imutáveis após `End()` → `SetName`/`SetAttributes` viram no-op silencioso.)
- **Ring Standard (observability):** *"telemetry middleware must be first; EndTracingSpans must be last."* A ordem do standard é necessária mas **não suficiente** — mesmo seguindo-a, o defeito ocorre. A lib precisa se apropriar da finalização; não pode depender da ordem do consumidor.
- **Ordenação interna do `WithTelemetry` já está correta** (confirmado no review): `applyTelemetrySpanAttributes` (`:245`) roda antes do `defer endState.End()` (`:226`), que só dispara no `return` (`:258`). O problema é exclusivamente o `EndTracingSpans` **externo** encerrando antes.
- **Options:**
  - **(a) `WithTelemetry` é o ÚNICO encerrador do seu span raiz + `EndTracingSpans` pula spans "owned".** `WithTelemetry` finaliza (nome+atributos+status) em `:245` e encerra via seu `defer` (`:226`); `EndTracingSpans` detecta que o `endState` pertence ao `WithTelemetry` e **não** o encerra. Robusto em todas as ordens.
  - **(b) Mover finalização para dentro do encerrador compartilhado** — viável mas força o `EndTracingSpans` a reconstruir estado da requisição (attrs/status/rota) que ele não possui, acoplando-o ao framework HTTP. Mais sujo.
  - **(c) Nomear na criação** — REJEITADA: template não confiável pré-`c.Next()`.
- **Decision:** **Opção (a), com a mecânica precisa que o review estabeleceu:**
  1. Adicionar um **sinal de ownership** ao `spanEndState` (ex.: flag `owned bool`, `:78-93`), setado pelo `WithTelemetry`.
  2. `WithTelemetry` é o **único** que encerra seu span raiz (via `defer`, `:226`), depois de finalizar em `:245`.
  3. `EndTracingSpans` (`:412-415`): quando o `endState` é `owned`, **não** chama `End()` (retorna). O fallback para spans estrangeiros (`trace.SpanFromContext(...).End()`, `:417-419`) é **preservado intacto**.
  4. Guardar o rename/attrs com `span.IsRecording()` — documenta a intenção "pular se já encerrado" (torna testável em vez de depender do no-op-após-End).
- **`sync.Once` NÃO resolve o hazard** (correção do review): `Once` só previne double-end; **não** garante encerrar *após* a finalização. A ordem é resolvida pelo flag `owned`, não pelo `Once`. O `Once` permanece como defesa contra double-end e para o caminho de span estrangeiro.
- **Rationale:** Torna o `WithTelemetry` autoritativo sobre o ciclo de vida do span que ele cria; elimina a corrida por design (single-goroutine + flag); mantém a capacidade legítima do `EndTracingSpans` de encerrar spans estrangeiros/handler-criados (há teste que depende — `TestEndTracingSpans_EndsFinalContextSpan`). Não depende da ordem do consumidor.
- **Consequences:**
  - `EndTracingSpans` deixa de ser quem encerra o span raiz do `WithTelemetry` (passa a no-op nesse caso) — mudança de comportamento observável interna; testes que asseram "EndTracingSpans encerra o root" precisam de ajuste.
  - `WithTelemetry` encerra o span raiz em **todos** os caminhos (normal/erro/panic/streaming/hijack) via seu `defer` — o review confirmou que `EndTracingSpans` não é estritamente necessário para o span raiz em nenhum desses.
  - Requer teste das **3 permutações posicionais** + caso "sem EndTracingSpans registrado" para provar robustez.
  - Aplicar semântica equivalente ao par gRPC (`WithTelemetryInterceptor`/`EndTracingSpansInterceptor`, `:476`/`:506`) para não divergir — **prioridade menor** (gRPC nomeia de `info.FullMethod`, sem PII/cardinalidade), pode ficar fora do escopo mínimo mas documentado.

### ADR-002 — Fonte e formato do nome do span

- **Context:** O nome precisa ser baixa cardinalidade e livre de PII — **inclusive no momento da CRIAÇÃO do span**, não só após o rename (correção do review: o sampler enxerga o nome da criação).
- **Decision:**
  - **Nome na criação (`:213/:223`) = só `{método}`** (ex.: `GET`), NÃO `{método} {caminho}`. Isso remove PII/cardinalidade do nome que o sampler vê e do caso de rota não reconhecida.
  - **Após `c.Next()`**, quando o roteamento reconheceu uma rota, renomear para `{método} {template de rota}` (via o mesmo helper que a métrica já usa).
  - **Fallback (404/catch-all):** mantém o nome só-método da criação; não força rota. (Alternativa: manter mascaramento de UUID como enriquecimento do fallback — decidir na task, mas o padrão é só-método.)
- **Rationale:** Convenção estável de indústria para spans HTTP server; exclui PII estruturalmente; alinha nome e métrica (US-4). **Ponto do review:** criar o span já com nome só-método garante que nem o sampler nem spans descartados nem rotas não-casadas carreguem o caminho cru com identificadores.
- **Consequences:** O mascaramento heurístico de UUID (`replaceUUIDWithPlaceholder`) **deixa de ser a defesa principal** — passa, no máximo, a enriquecimento opcional do fallback. Requisições sem rota reconhecida terão nome só-método (correto e de baixa cardinalidade).
- **Caveat de sampling (documentar na release):** o rename pós-criação NÃO reavalia a decisão de amostragem (feita na criação, com o nome só-método agora). Consumidores que amostram por nome de span devem ser avisados — mas com nome só-método na criação, não há PII exposta a samplers.

### ADR-003 — Distinção da convenção de nomeação (evitar conflito com o standard)

- **Context:** A Ring Standard exige spans no padrão `layer.domain.operation` (ex.: `service.tenant.create`). O span aqui é o **span raiz HTTP server** criado pelo middleware, não um span de camada de serviço/repositório.
- **Decision:** Spans **raiz HTTP server** seguem a convenção OTel `{método} {http.route}` (a própria standard descreve o middleware criando "root spans for HTTP endpoints"). O padrão `layer.domain.operation` continua válido para spans **internos** (serviço/repositório) criados pelos handlers — **fora do escopo** desta mudança.
- **Rationale:** As duas convenções não conflitam; aplicam-se a camadas diferentes. Documentar evita interpretação equivocada de "violação de standard".

### ADR-004 — Escopo adicional (decisões deferidas do PRD)

- **Ponto de extensão de nomeação (hook público de formatação):** **Não incluir agora.** Não há demanda de consumidor; ampliaria a superfície pública da API sem necessidade comprovada. Reavaliar se surgir caso de uso.
- **Alinhamento de convenções internas de atributo (literais → helpers canônicos):** **Incluir como oportunístico** apenas se trivial e baixo risco (o review endossou). Não bloqueante.
- **Reclassificação de error.type (business vs technical):** **NÃO incluir** (correção do review). O código atual marca status ERROR para qualquer erro de handler e ≥500; a reclassificação business/technical é uma mudança de **contrato observável separada** e não deve ser embutida neste fix cirúrgico. Registrar como to-do próprio se relevante.

---

## 3. Qualidade / Testes

Conforme golang/quality.md e as convenções do repo (research §9):
- **Atualizar** os testes que travam o nome antigo (caminho concreto) para asserir o **nome baseado em template**, espelhando o teste já existente da métrica.
- **Adicionar** testes unitários isolados para o helper de rota e para o mascaramento de identificadores (hoje inexistentes).
- **Testes-chave de robustez (novos, exigidos pelo review):**
  - Nome+atributos aplicados corretamente e span encerrado **exatamente uma vez** nas **3 permutações posicionais**: (i) `WithTelemetry` primeiro + `EndTracingSpans` por último (ordem do standard), (ii) `EndTracingSpans` antes das rotas (ordem do plugin), (iii) `EndTracingSpans` fora/antes do `WithTelemetry`.
  - Span raiz encerrado corretamente **sem `EndTracingSpans` registrado** (prova que o `WithTelemetry` é autossuficiente).
  - Span **estrangeiro/handler-criado** ainda encerrado pelo `EndTracingSpans` (preserva `TestEndTracingSpans_EndsFinalContextSpan` — não quebrar o fallback).
- **Nome PII-free na criação:** asserir que o nome na criação do span é só-método (não contém caminho), garantindo que o sampler não vê PII.
- **Fallback:** testar rota não reconhecida (nome só-método, `http.route` omitido).
- Manter tag de build de teste unitário e mocks gerados conforme o repo. Linters obrigatórios (14) devem passar; `forbidigo` (sem `fmt.Print`/`panic` em lib) respeitado.

### Caveats técnicos a documentar (research)
- **Reciclagem de buffer do framework:** strings da requisição precisam ser copiadas antes do roteamento; o template de rota é seguro após o roteamento (não é buffer de requisição).
- **Renomear span após criação é válido** no SDK em uso (no-op se o span não estiver sendo gravado — guardar se necessário).
- **Amostragem (sampling):** a decisão de amostragem é feita na criação com o nome original e **não** é reavaliada no rename. Se algum consumidor usa amostragem por nome de span, deve ser avisado (documentar na nota de release).

---

## 4. Versionamento / Rollout (implicação de arquitetura, mecânica é processo)
- Mudança do formato do nome do span é **breaking change** observável (dashboards/buscas que agrupam pelo nome antigo). Deve ser sinalizada como tal no processo de release da biblioteca (pré-lançamento antes de estável) e comunicada aos consumidores.
- **Dependência com a feature do consumidor (plugin):** o ADR-001 torna a lib robusta mesmo com ordem errada, mas a correção de ordem no consumidor (feature separada) continua recomendada por conformidade com o standard. A lib **não depende** dela para funcionar.

---

## Gate 2 — Validação
| Categoria | Status |
|---|---|
| Todos os requisitos do PRD mapeados | ✅ (US-1..US-4) |
| Fronteiras de componente claras | ✅ (WithTelemetry / EndTracingSpans / routeAttribute) |
| Hazard central resolvido por design | ✅ (ADR-001, opção a, revisada) |
| Review de engenheiro Go do ADR-001 | ✅ SÓLIDA + 4 correções aplicadas |
| Estratégia de qualidade/testes | ✅ (3 permutações de ordem + nome PII-free) |
| Quality attributes atingíveis | ✅ |
| Decisões deferidas do PRD resolvidas | ✅ (ADR-002/003/004) |
| Caveats técnicos documentados | ✅ (buffer, sampling, rename, ownership) |

**Resultado:** ✅ PASS → Gate 3 (tasks)
