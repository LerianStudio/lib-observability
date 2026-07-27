# Gate 6 — Dependency Map: span-kind-client-helpers

> Gates anteriores: [research.md](./research.md) · [trd.md](./trd.md) · [api-design.md](./api-design.md) · [data-model.md](./data-model.md)
> Data: 2026-07-27 · Track: Full · Status: draft (aguardando aprovação)
> **Gate central desta feature** (motivo do full track): a única dependência nova a pinar.

## Step 0 — Standards & Project Rules

- **TRD tech decisions:** lidas do metadata do [trd.md](./trd.md) — stack Go (library), deployment N/A, sem auth/license.
- **Ring Standards:** `golang.md` carregado (Gate 3 + Gate 6). Regra relevante para lib: **minimizar dependências transitivas** (dep pesada onera o consumidor). → 1 direta + 1 transitiva é aceitável e mínimo.
- **PROJECT_RULES:** esta biblioteca **não usa `docs/PROJECT_RULES.md`**. As regras de tecnologia/versão vivem em `go.mod` (lock real), `CLAUDE.md` (layout/commit/lint/release) e `docs/metrics-contract.md` (contrato de métricas). Estes cumprem o papel de PROJECT_RULES; este dependency-map é o **registro de decisão** da dep nova. Não criar arquivo redundante (mesma justificativa do TRD Step 0).

---

## 1. Mapa componente → tecnologia

| Componente (TRD) | Requisito (PRD) | Tecnologia concreta | Versão | Dep nova? |
|---|---|---|---|---|
| C1 wrapper HTTP-saída (`httpobs`) | F1/F4 (classificar CLIENT + medir `http.client.request.duration`) | `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | **v0.69.0** | **Sim (direta)** |
| C1 (transitiva) | — | `github.com/felixge/httpsnoop` | **v1.0.4** | **Sim (transitiva)** |
| C2 helper de span (`tracing.StartClientSpan`) | F2 | `go.opentelemetry.io/otel/trace` | v1.44.0 | Não (já presente) |
| C3 convenção/docs | F3 | — (documentação) | — | Não |

---

## 2. Dependência direta nova — `otelhttp`

**Package:** `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`
**Version (pinned, exato):** `v0.69.0`
**Purpose:** instrumentação HTTP-cliente de mercado — o `httpobs.NewTransport` é uma casca fina sobre `otelhttp.NewTransport`. Emite span CLIENT + `http.client.request.duration` (semconv) + propagação de contexto. Não reinventar.

### Justificativa da versão (VERIFICADO na fonte, não inferido)
Lido o `go.mod` da própria tag `instrumentation/net/http/otelhttp/v0.69.0`:
```
go 1.25.0
require (
    github.com/felixge/httpsnoop v1.0.4
    go.opentelemetry.io/otel        v1.44.0
    go.opentelemetry.io/otel/metric v1.44.0
    go.opentelemetry.io/otel/sdk    v1.44.0
    go.opentelemetry.io/otel/sdk/metric v1.44.0
    go.opentelemetry.io/otel/trace  v1.44.0
    github.com/stretchr/testify v1.11.1   // test-only
)
```
- **Requer otel core `v1.44.0` EXATO** = idêntico ao que já está no `go.mod` da lib → **zero bump do otel core**. Todos os `otel/*` que o otelhttp requer (otel, metric, sdk, sdk/metric, trace) **já estão presentes em v1.44.0**.
- **Mesmo release-train** do `instrumentation/runtime v0.69.0` **já presente** no `go.mod` → consistência de train (um só v0.x de contrib).
- **Go 1.25.0** mínimo — satisfeito (lib usa Go 1.26.3).
- **Nome de métrica estável:** em v0.69.0 emite `http.client.request.duration` em **segundos** por default (semconv v1.41.0/httpconv). O legado `http.client.duration` (ms) só aparece sob `OTEL_SEMCONV_STABILITY_OPT_IN=http/dup` — **não usar**. Buckets default batem com `metrics-contract.md:21`. → sem conversão ms, sem dual-emit.

### Segurança
- **CVE:** sem CVE conhecido para `otelhttp` v0.69.0 (pacote oficial OTel, amplamente usado, mantido ativamente; release do train v1.44.0/2026-05-28). Verificado 2026-07-27.
- **Supply chain:** repositório oficial `open-telemetry/opentelemetry-go-contrib` (multi-maintainer, CNCF). Baixo risco.
- **Política de update:** acompanhar o train do otel core; só bumpar `otelhttp` junto com um bump coordenado de `otel` (senão quebra o pin exato). Dependabot/renovate, se houver, deve tratar otelhttp+otel como grupo.

### Licença
- **Apache-2.0** (repositório OTel-contrib). Permissiva, compatível com o uso comercial da lib. Sem GPL.

### Estabilidade
- otelhttp é **v0.x** (API pode mudar em minor) — por isso **pin exato**, sem range/`^`. É a instrumentação HTTP canônica e estável no uso diário.

---

## 3. Dependência transitiva nova — `httpsnoop`

**Package:** `github.com/felixge/httpsnoop`
**Version:** `v1.0.4` (puxada por `otelhttp`; entra como `// indirect`).
**Purpose:** captura de status/bytes da resposta HTTP (usado internamente pelo otelhttp).
**Presença hoje:** ausente do `go.sum` (confirmado: `grep felixge/httpsnoop go.sum` = 0). `go mod tidy` a adiciona.
**Segurança:** biblioteca madura, estável (v1.0.x de longa data), sem CVE conhecido (verificado 2026-07-27).
**Licença:** **MIT**. Permissiva, sem GPL.
**Observação (Ring golang.md):** é a **única** dep transitiva realmente nova — otelhttp não puxa nenhum outro `otel/*` além dos já presentes. Impacto na árvore de dependências do consumidor = mínimo (1 pacote pequeno).

> Confirmar após `go mod tidy`: se aparecer qualquer transitiva além de `httpsnoop v1.0.4`, revisar antes do commit `deps` (esperado: só ela).

---

## 4. Sem nova dep — `StartClientSpan`

`tracing.StartClientSpan` usa apenas `go.opentelemetry.io/otel/trace` (v1.44.0, **já presente**). Zero dependência nova. Por isso o helper C2 pode shipar **antes/independente** do httpobs (relevante para o faseamento do Gate 7).

---

## 5. Alternativas avaliadas

| Alternativa | Trade-off | Veredito |
|---|---|---|
| **Reimplementar o transport manualmente** (span+métrica+propagação à mão) | Reinventa código sensível (semconv, propagação W3C, encerramento de span no close do body); mais superfície de bug; diverge do padrão "thin wrapper" da lib | ❌ Rejeitada — otelhttp já é o padrão de mercado e é o que o wrapper deve delegar |
| **otelhttp mais novo** (train > v0.69.0) | Puxaria `otel` core **> v1.44.0** → bump coordenado de todo o otel core da lib (fora do escopo desta feature; risco de regressão) | ❌ Rejeitada — manter o train pinado ao otel v1.44.0 já presente |
| **Pin flutuante** (`^v0.69` / `latest`) | Build não-reproduzível; otelhttp é v0.x (pode quebrar em minor) | ❌ Rejeitada — pin exato `v0.69.0` |
| **StartClientSpan manual também p/ HTTP** (não adotar otelhttp) | Não emitiria `http.client.request.duration` (perderia F1/F4); reintroduz risco de span-name/cardinalidade sem os guardrails do otelhttp | ❌ Rejeitada p/ HTTP — StartClientSpan é só p/ saídas SEM wrapper (ADR-006) |

---

## 6. Matriz de compatibilidade

| Aspecto | Restrição | Status |
|---|---|---|
| `otelhttp v0.69.0` ↔ `otel` core | requer v1.44.0 **exato** | ✅ igual ao go.mod atual |
| `otelhttp v0.69.0` ↔ `otel/metric,sdk,sdk/metric,trace` | v1.44.0 | ✅ já presentes |
| `otelhttp v0.69.0` ↔ Go | ≥ 1.25.0 | ✅ lib usa 1.26.3 |
| Nome/unidade `http.client.request.duration` (s) | default em v0.69.0 | ✅ = metrics-contract.md:29 |
| Buckets advisory (0.005…10 s) | default httpconv | ✅ = metrics-contract.md:21 |
| Novas transitivas | só `felixge/httpsnoop v1.0.4` | ✅ mínimo |
| `otelhttp` ↔ `instrumentation/runtime v0.69.0` (já presente) | mesmo train | ✅ consistente |

**Sem conflitos de versão. Sem CVE crítico/alto. Licenças permissivas.**

---

## 7. Convenções de commit / scope de PR (decisão registrada)

Allowlist do CI (`.github/workflows/pr-validation.yml` `pr_title_scopes`): `assert, ci, config, constants, core, deps, docs, log, metrics, middleware, redaction, runtime, scripts, tests, tracing, zap`. **NÃO há scope `httpobs`** (CI rejeita) e não há scope por-pacote-obs (sqlobs/redisobs também não têm).

| Entregável | Scope de commit/PR | Justificativa |
|---|---|---|
| Adição de `otelhttp v0.69.0` + `go mod tidy` | **`deps`** (ex.: `chore(deps): add otelhttp v0.69.0 for http client instrumentation`) | scope dedicado a dependências, na allowlist |
| `tracing.StartClientSpan` | **`feat(tracing)`** | `tracing` na allowlist; span é concern de tracing |
| Pacote `httpobs` | **`feat(metrics)`** (precedente sqlobs/redisobs) OU `feat(core)` | `httpobs` NÃO está na allowlist → usar precedente. **Decisão:** `feat(metrics)` (mesma família das medições, precedente direto de sqlobs/redisobs) |
| Docs README/precedência (C3) | **`docs`** | scope na allowlist |

> Alternativa considerada: **expandir a allowlist para incluir `httpobs`** (edição de `pr-validation.yml`, scope `ci`, no mesmo PR). **Rejeitada por ora** — segue o precedente `feat(metrics)` de sqlobs/redisobs para não divergir; reabrir só se o time quiser scopes por-pacote-obs de forma geral (aí migra sqlobs/redisobs/messagingobs junto — follow-up).

Commits: GPG `-S` + trailer `X-Lerian-Ref: 0x1` + Conventional (CLAUDE.md).

---

## 8. Custo & licença (resumo)

- **Custo de infraestrutura:** R$ 0 — é biblioteca; sem runtime/infra própria. (Impacto de custo *positivo* downstream: destrava redução de cardinalidade/telemetria — PRD §5, fora deste gate.)
- **Licenças novas:** Apache-2.0 (otelhttp) + MIT (httpsnoop) — ambas permissivas, sem obrigação de atribuição especial além do padrão OTel/MIT já presente na árvore. Sem GPL.

---

## Gate 6 — Validação

- [x] **Standards (hard block):** Ring golang.md carregado; regra de minimizar transitivas respeitada (1+1)
- [x] PROJECT_RULES: papel cumprido por go.mod+CLAUDE.md+metrics-contract.md (justificado); decisões registradas aqui
- [x] Toda dep com versão **explícita e exata** (otelhttp v0.69.0, httpsnoop v1.0.4) — sem `latest`/range
- [x] Matriz de versão completa; sem conflitos; runtime (Go ≥1.25) especificado
- [x] Segurança: sem CVE crítico/alto; supply chain oficial OTel + MIT maduro; política de update documentada
- [x] Licenças compatíveis (Apache-2.0, MIT); sem GPL
- [x] Todo componente do TRD mapeado a tecnologia (C1→otelhttp, C2→otel/trace já presente, C3→docs)
- [x] Alternativas avaliadas com trade-off (§5)
- [x] Decisão de scope de PR registrada (§7)
- [x] Custo documentado (§8)
- [x] Verificação feita na FONTE (go.mod da tag), não só via agente

**Confidence:** Familiaridade 30 (OTel já é o stack) · Compatibilidade 25 (verificada na fonte) · Segurança 25 (scan + supply chain oficial) · Custo 20 (detalhado: R$0) = **100/100 → autônomo**.

**Resultado do Gate:** ✅ PASS (sujeito à aprovação humana)

---

## Ação pós-aprovação
1. 🔒 Pin travado: `otelhttp v0.69.0` (+ `httpsnoop v1.0.4` indirect).
2. No PR de implementação: `go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.69.0` && `go mod tidy`; **verificar** que só `httpsnoop v1.0.4` foi adicionado como transitiva nova.
3. Commit da dep sob `deps`, separado do commit de código.
