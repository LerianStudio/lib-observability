# Gate 4 — API Design: span-kind-client-helpers

> Gates anteriores: [research.md](./research.md) · [prd.md](./prd.md) · [feature-map.md](./feature-map.md) · [trd.md](./trd.md)
> Data: 2026-07-27 · Track: Full · Status: draft (aguardando aprovação)
> Contratos = **API Go pública in-process** (biblioteca). Versões/pacotes concretos → Gate 6. Nomes finais de campo/func já fixados aqui (é o contrato do pacote público).

## Overview

Dois contratos públicos novos + uma superfície documental:
- **C1 — pacote `httpobs`** (novo): instrumenta saída HTTP → span CLIENT + medição de duração por dependência.
- **C2 — `tracing.StartClientSpan`** (adição ao pacote `tracing` existente): inicia span já classificado CLIENT.
- **C3 — convenção** (doc.go/README): princípio de precedência (não é API executável; contrato de conteúdo).

## Versioning Strategy

- SemVer via release da própria biblioteca (beta no canal de desenvolvimento, estável no principal).
- **Aditivo** = minor. Nenhum símbolo existente muda de assinatura → sem breaking change.
- Opções são funcionais (`Option`), então novas opções no futuro são não-quebra.
- Compatibilidade retroativa: apps que não importam `httpobs` nem chamam `StartClientSpan` não são afetadas.

---

## C1 — Contrato do pacote `httpobs`

**Purpose:** dado um meio de transporte HTTP, devolver um instrumentado que classifica cada saída como CLIENT, emite `http.client.request.duration` e propaga contexto de rastreamento — degradando para no-op quando a telemetria está desligada. Casca fina sobre a instrumentação HTTP-cliente de mercado (ADR thin-wrapper), aplicando os defaults corporativos (guardrails).

**Integration points:** inbound = a app fornece o transporte base + opções; outbound = transporte/cliente instrumentado + telemetria em runtime; consome o contrato de métricas (conformidade) e os provedores injetados.

### Operação `NewTransport`

- **Purpose:** envolver um meio de transporte base, retornando um instrumentado.
- **Assinatura conceitual:**
  ```
  NewTransport(base RoundTripper, opts ...Option) RoundTripper
  ```
- **Inputs:**

| Parâmetro | Tipo | Obrigatório | Restrições | Descrição |
|---|---|---|---|---|
| `base` | RoundTripper | Não | `nil` ⇒ usa o transporte padrão da plataforma | Transporte base da app (preserva TLS/timeout/proxy custom) |
| `opts` | Option (variádico) | Não | ver tabela de Options | Configuração (provedores, propagação, nome, atributos) |

- **Output (sucesso):** um `RoundTripper` instrumentado, pronto para ser injetado no cliente HTTP da app.
- **Comportamento em runtime (por chamada):** cria span CLIENT (se `TracerProvider` injetado — ADR-005), mede duração conforme contrato, injeta contexto de rastreamento nos cabeçalhos de saída. O span encerra quando a app lê/fecha o corpo da resposta (documentar).
- **Errors:** nenhum erro retornado — `base==nil` **não é erro** (cai no transporte padrão). Construção é infalível (alinha ADR-005 no-op).
- **Idempotência:** envolver duas vezes = dupla-instrumentação (anti-pattern) → documentar; a operação em si é pura (não muta `base`).

### Operação `NewClient` (conveniência)

- **Purpose:** atalho que devolve um cliente HTTP já com o transporte instrumentado, **preservando** o transporte base da app.
- **Decisão (questão aberta resolvida):** assinatura **recebe `base` + `opts`**, não só `opts`.
  ```
  NewClient(base RoundTripper, opts ...Option) *HTTPClient
  ```
- **Rationale:** o research apontou que passar só `opts` obrigaria a descartar o transporte base (e com ele a config de TLS/timeout/proxy da app) — bug sutil. Recebendo `base`, `NewClient(base, opts...)` = `&HTTPClient{Transport: NewTransport(base, opts...)}`. `base==nil` ⇒ transporte padrão (mesma regra do `NewTransport`).
- **Inputs:** iguais ao `NewTransport`.
- **Output:** um cliente HTTP pronto para uso.
- **Errors:** nenhum.
- **Nota:** `NewClient` é açúcar; apps que já montam o próprio cliente devem usar `NewTransport` e atribuir ao campo de transporte.

### Options (functional options — padrão sqlobs/redisobs)

| Option | Assinatura conceitual | Default | Descrição |
|---|---|---|---|
| `WithMeterProvider` | `WithMeterProvider(mp MeterProvider) Option` | provedor global | Provedor da medição `http.client.request.duration`. `nil` ignorado (não sobrescreve). |
| `WithTracerProvider` | `WithTracerProvider(tp TracerProvider) Option` | **nenhum** (span só sai se injetado — ADR-005) | Provedor do span CLIENT. Sem ele, não há span (mas pode haver medição). O caminho "telemetria on" da lib DEVE injetá-lo. `nil` ignorado. |
| `WithPropagators` | `WithPropagators(p Propagator) Option` | propagador global | Propagação de contexto nos cabeçalhos de saída. `nil` ignorado. |
| `WithSpanNameFormatter` | `WithSpanNameFormatter(fn func(op string, req *Request) string) Option` | **limitado por padrão** (só método, ex.: "HTTP GET") | Formata o nome da operação. Guardrail: default NÃO inclui caminho concreto. Documentar risco de cardinalidade se o caller injetar caminho com id. |
| `WithAttributes` | `WithAttributes(attrs ...Attribute) Option` | vazio | Atributos extras **bounded, PII-free** (mesma disclaimer de `sqlobs.WithAttributes`). Caminho/consulta de URL e dado pessoal são PROIBIDOS e nunca adicionados pela lib. |

- **`config` interno + `newConfig(opts...)`** com defaults resolvidos (provedores globais), espelhando `redisobs.newConfig`. Cada `WithX` faz nil-guard.
- **Superfície mínima confirmada** (não expor mais que isto; `WithMetricAttributesFn` da lib de mercado é deprecated → não repassar).

### Contrato de telemetria emitida (conformidade — ADR-007)

| Sinal | Nome | Tipo | Unidade | Rótulos (valores literais canônicos) | Fonte |
|---|---|---|---|---|---|
| Medição de duração por dependência | `http.client.request.duration` | Histogram | `s` | `http.request.method`, `http.response.status_code`, `server.address`, `error.type` | metrics-contract.md:29 |
| Span de saída | (nome pelo formatter, ex.: "HTTP GET") | Span | — | classificação = **CLIENT** | ADR-004/007 |

- **Buckets:** os mesmos advisory de HTTP já usados no produtor servidor (metrics-contract.md:21).
- **Guardrail (ADR-007):** rótulos são **valores literais canônicos**, espelhando o produtor servidor existente; `url.path`/`url.query`/PII **nunca** viram rótulo. Verificado por teste anti-vazamento (espelha sqlobs/redisobs).

### doc.go (contrato de conteúdo — 4 seções ADR, padrão redisobs/sqlobs)
1. Preâmbulo: thin, nil-safe wrapper de saída HTTP.
2. **Boundary:** não cria/possui o cliente HTTP da app; só envolve o transporte; não dispara requisições. **Precedência (C3):** "prefira este wrapper para saída HTTP; use `tracing.StartClientSpan` só para saídas sem wrapper".
3. **Emitted telemetry:** `http.client.request.duration` (s) + span CLIENT.
4. **PII / cardinality guardrail (docs/metrics-contract.md):** url.path/query nunca como rótulo; nome limitado por padrão; "Enforced by tests".
5. **No-op degradation:** base nil ⇒ transporte padrão; provedores ausentes ⇒ no-op; span só com TracerProvider (ADR-005).

---

## C2 — Contrato de `tracing.StartClientSpan`

**Purpose:** iniciar um span já classificado como CLIENT sobre o rastreador existente, sem o chamador precisar conhecer o detalhe de baixo nível. Menor átomo possível (só injeta a classificação; não emite métrica, não impõe contrato de nome/atributo).

### Operação `StartClientSpan`

- **Assinatura conceitual:**
  ```
  StartClientSpan(ctx Context, tracer Tracer, name string, opts ...SpanStartOption) (Context, Span)
  ```
- **Inputs:**

| Parâmetro | Tipo | Obrigatório | Restrições | Descrição |
|---|---|---|---|---|
| `ctx` | Context | Sim | — | Contexto pai |
| `tracer` | Tracer | Sim | obtido pelos meios existentes da lib (`NewTrackingFromContext`) | Rastreador cru sobre o qual o span é criado |
| `name` | string | Sim | **low-cardinality, PII-free** (responsabilidade do caller — documentar) | Nome da operação |
| `opts` | SpanStartOption (variádico) | Não | — | Opções de span do chamador |

- **Comportamento (ADR-004 — default sobreponível):** **prepend** de `WithSpanKind(Client)` aos `opts` do caller: `opts = [WithSpanKind(Client)] ++ opts`. Como a plataforma resolve opções por "última vence", uma classificação explícita do caller (informada depois) prevalece. Nunca força.
- **Output (sucesso):** `(Context derivado, Span iniciado com classificação CLIENT)`.
- **Errors:** nenhum retornado. Contratos de nil-safety do `tracer` seguem a semântica do rastreador subjacente (a lib já normaliza tracer via `resolveTracer`); documentar que `tracer` deve vir dos meios da lib.
- **Boundary:** NÃO emite métrica; NÃO valida `name`/atributos quanto a PII/cardinalidade (caller é dono — disclaimer no doc-comment, estilo `sqlobs.WithAttributes`).
- **Escopo (decisão):** expõe **apenas** `StartClientSpan`. `StartServerSpan`/`StartInternalSpan` **não** são expostos — Server já é coberto pelos middlewares; Internal é o default do rastreador (sem valor). Producer/Consumer já são cobertos por `messagingobs`.

### doc-comment (parte de C3 — precedência inline)
O doc-comment de `StartClientSpan` deve conter, ali mesmo, o aviso de precedência: "prefira o wrapper específico se existir (`sqlobs`/`redisobs`/`httpobs`/middleware/`messagingobs`); use este helper **apenas** para saídas sem wrapper — ex.: banco de documentos (ADR-003), chamada a serviço custom. Não instrumente em dobro (ADR-006)."

---

## C3 — Contrato de conteúdo (convenção de precedência)

Não é API executável — é requisito de conteúdo (CA4/CA5), verificável por revisão. Presente e consistente em **três** lugares:

| Local | Conteúdo exigido |
|---|---|
| README / doc de uso da lib | Tabela/parágrafo do princípio de precedência: para cada tipo de saída (SQL/cache/HTTP/mensageria/servidor) → o wrapper correspondente; "manual só sem wrapper"; alerta anti-dobro. |
| `doc.go` de cada pacote de instrumentação (incl. `httpobs` novo) | Seção Boundary com a frase de precedência apontando para o wrapper e para o helper manual. |
| doc-comment de `tracing.StartClientSpan` | Aviso inline (acima) + exemplos de quando o manual é legítimo. |

---

## Custom Type Definitions (abstratas — tipos concretos no Gate 6)

| Tipo (papel) | Base conceitual | Observação |
|---|---|---|
| RoundTripper | meio de transporte HTTP | tipo padrão da plataforma HTTP |
| MeterProvider / TracerProvider / Propagator | provedores de telemetria | injetáveis; default = globais |
| Option | functional option de `httpobs` | `func(*config)` (padrão sqlobs/redisobs) |
| SpanStartOption | opção de início de span | tipo da plataforma de rastreamento |
| Attribute | par chave-valor bounded | PII-free obrigatório |

## Naming Conventions
- Construtores: `New<Coisa>` (`NewTransport`, `NewClient`). Helper de span: `Start<Kind>Span` (`StartClientSpan`).
- Options: `With<Coisa>`. Config interna: `config` + `newConfig` (não exportados).
- Sentinel error (se necessário): `Err<Condição>` (ex.: só se `NewClient` puder falhar — hoje não pode).

## Testing Contracts (contrato de teste — espelha sqlobs/redisobs)
- **Conformidade de métrica:** `http.client.request.duration` emitida, unidade `s`, rótulos exatos do contrato. (harness ManualReader + exporter em memória).
- **Anti-vazamento:** requisição a URL com id/PII no caminho/consulta ⇒ asserir que NÃO aparece como rótulo da métrica nem atributo do span.
- **Classificação:** span de saída = CLIENT; `StartClientSpan` produz CLIENT por default e respeita override do caller (ADR-004).
- **No-op:** sem provedores ⇒ sem panic, sem efeito; `base==nil` ⇒ usa transporte padrão.
- **Precedência (C3):** revisão confirma a frase nos três locais + exemplo.
- Build tag `unit`; table-driven onde couber.

---

## Gate 4 — Validação

- [x] Todas as interações componente-a-componente têm contrato (C1, C2) + contrato de conteúdo (C3)
- [x] Operações com propósito claro e naming consistente (New*, StartClientSpan, With*)
- [x] Inputs tipados, obrigatório vs opcional explícito; nil-cases tratados (base nil, providers nil)
- [x] Erros identificados (nenhum retornado — justificado por nil-safe/no-op; sentinel só se necessário)
- [x] Contrato de telemetria emitida especificado (nome/unidade/rótulos/buckets) conforme metrics-contract.md
- [x] Guardrails contratados (ADR-005 provider→span; ADR-007 rótulos literais + anti-PII)
- [x] Questão aberta resolvida: `NewClient(base, opts...)` (preserva TLS)
- [x] Decisão de escopo: só `StartClientSpan` (sem Server/Internal)
- [x] Versionamento aditivo/semver; retrocompat garantida
- [x] Technology-agnostic no nível de protocolo (é API in-process de lib; sem REST/serialização); versões de pacote → Gate 6

**Confidence:** Contract completeness 30 · Interface clarity 25 · Integration complexity 25 (ponto-a-ponto simples) · Error handling 20 = **100/100 → autônomo**.

**Resultado do Gate:** ✅ PASS (sujeito à aprovação humana)
