# Gate 5 — Data Model: span-kind-client-helpers

> Gates anteriores: [research.md](./research.md) · [prd.md](./prd.md) · [feature-map.md](./feature-map.md) · [trd.md](./trd.md) · [api-design.md](./api-design.md)
> Data: 2026-07-27 · Track: Full · Status: draft (aguardando aprovação)

## Veredito: **N/A justificado — sem entidades persistidas**

Esta feature é uma **biblioteca de telemetria in-process**. Não introduz nem toca:
- ❌ entidades de domínio persistidas
- ❌ banco de dados / esquema / migração
- ❌ estado compartilhado durável
- ❌ relacionamentos entre entidades
- ❌ padrões de acesso a dados (consulta/escrita)

Portanto **não há modelo de dados no sentido do Gate 5** (entidades, ownership de dados, estratégia de consistência, migração). Forçar entidades artificiais aqui violaria o princípio do gate. O que existe são **estruturas efêmeras de configuração** (em memória, por construção) e o **formato da telemetria emitida** — ambos já contratados no Gate 4. Documentados abaixo por completude, explicitamente marcados como não-persistidos.

---

## 1. Estruturas efêmeras (configuração — não persistidas)

Vivem apenas em memória, criadas na construção do wrapper e descartadas com ele. Não são entidades — são valores de configuração resolvidos (padrão `config`+`newConfig` de sqlobs/redisobs).

### `httpobs.config` (não exportada)
| Campo (papel) | Origem | Ciclo de vida | Persistência |
|---|---|---|---|
| provedor de medição | `WithMeterProvider` / global | criado 1× na construção | efêmero |
| provedor de rastreamento | `WithTracerProvider` / ausente | criado 1× na construção | efêmero |
| propagador | `WithPropagators` / global | 1× | efêmero |
| formatador de nome | `WithSpanNameFormatter` / default limitado | 1× | efêmero |
| atributos extras (bounded) | `WithAttributes` | 1× | efêmero |

- **Ownership:** interna ao pacote `httpobs`; nunca exposta.
- **Consistência:** irrelevante (imutável após `newConfig`; sem concorrência de escrita).
- **Acesso:** só leitura, no caminho da requisição.

`StartClientSpan` (C2) **não tem estrutura de configuração** — recebe tudo por parâmetro; nada retido.

---

## 2. Formato da telemetria emitida (dado transiente — já contratado no Gate 4)

Não é dado armazenado pela lib: é emitido e sai pelo pipeline de telemetria do consumidor. Reproduzido aqui só como referência de "formato de dado".

### Medição `http.client.request.duration`
| Atributo | Tipo | Cardinalidade | Guardrail |
|---|---|---|---|
| `http.request.method` | string enumerada | baixa (verbos) | ok |
| `http.response.status_code` | inteiro | baixa (códigos) | ok |
| `server.address` | string (host destino) | **potencialmente alta** se muitos destinos | risco documentado; normalizar/filtrar no consumo |
| `error.type` | string enumerada | baixa | ok |

- Unidade: `s`. Buckets: advisory HTTP (metrics-contract.md:21). Valores = literais canônicos (ADR-007).
- **PROIBIDO** como atributo: `url.path`/`url.query`, PII (metrics-contract.md:40-47) — nunca emitido (guardrail testado).

### Span de saída
| Propriedade | Valor |
|---|---|
| classificação | **CLIENT** (ADR-004) |
| nome | pelo formatador; default limitado (ex.: "HTTP GET") — sem caminho concreto |
| atributos | conjunto da instrumentação de mercado, filtrado pelo guardrail (sem PII/caminho) |

---

## 3. Migração / versionamento de dados

**Nenhuma.** Feature aditiva, sem esquema, sem estado durável → não há migração de dados. Versionamento é de **código** (SemVer da lib), coberto no Gate 4.

---

## Gate 5 — Validação

- [x] Entidades definidas com relacionamentos → **N/A justificado** (sem entidades persistidas; estruturas efêmeras documentadas)
- [x] Ownership de dados claro → config interna a `httpobs`, nunca exposta; telemetria pertence ao pipeline do consumidor
- [x] Padrões de acesso documentados → só leitura da config no caminho da requisição; sem escrita concorrente
- [x] Database-agnostic → N/A (sem banco); nenhum produto de dados nomeado
- [x] Guardrail de cardinalidade/PII do dado emitido documentado (server.address risco; url.path proibido)

**Confidence:** alta — a ausência de modelo de dados é uma característica correta de uma lib de telemetria, não uma lacuna.

**Resultado do Gate:** ✅ PASS (N/A justificado; sujeito à aprovação humana)
