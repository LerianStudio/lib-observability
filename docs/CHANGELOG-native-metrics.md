# O que está subindo — Native Metrics + Auto-Instrumentation (lib-observability)

> Registro vivo do que este trabalho adiciona à lib. Vira o corpo do PR único p/ develop.
> Branch: `feat/native-metrics-phase1` (base develop pós-#30). Atualizar a cada feature.
> Ref: pre-dev `docs/pre-dev/observability-metrics-standardization/` · contrato `docs/metrics-contract.md`

## Objetivo
Padronizar a emissão de métricas transversais (a lib emite por default, nome/unidade/labels iguais p/ todo serviço) e oferecer helpers de auto-instrumentação de DB/cache/fila — para acabar com a bagunça de métricas por-app e aposentar spans manuais de infra (origem de ~27k séries de alta cardinalidade).

## Não confundir
- **PR #30 (já mergeado)** = fix de span.name (route template). Pré-requisito, NÃO faz parte deste trabalho.
- **Adoção nas apps (midaz/ledger)** = outro repo, outro PR (Fase 3). Este PR é SÓ a lib.

---

## FASE 1 — Métricas nativas (✅ implementada, testada)

| Feature | Métrica | Tipo | Detalhe |
|---|---|---|---|
| T1 Contrato | — | doc | `docs/metrics-contract.md`: nomes/unidade(s)/buckets/labels permitidos×proibidos/no-op. Fundação. |
| T2 RED gRPC server | `rpc.server.duration` | Float64 Histogram (s) | hook no interceptor server existente. Labels: rpc.system=grpc, rpc.method, rpc.grpc.status_code, error.type (só !=OK), tenant.id |
| T2 RED gRPC client | `rpc.client.duration` | Float64 Histogram (s) | NOVO UnaryClientInterceptor (não existia). Labels iguais, sem tenant.id. Propaga trace. |
| T3 Runtime Go | `go.*` (memory/goroutine/gc) | contrib/runtime | opt-in via `TelemetryConfig.EnableRuntimeMetrics` (default-off). MinReadMemStats 15s. |
| T4 In-flight HTTP | `http.server.active_requests` | Int64 UpDownCounter ({request}) | inc antes / dec depois. Label http.request.method. Detecta saturação. |

Arquivos: middleware/telemetry.go, tracing/otel.go, go.mod (+contrib/instrumentation/runtime v0.69.0), 3 test files (16 testes). Verificado: go test unit PASS, vet OK, lint 0, ManualReader (emissão real), zero label proibido, no-op safe.

## DESACOPLAMENTO Fiber v3 ↔ core (✅ implementado, testado) — BREAKING

Objetivo: tirar a dependência de `github.com/gofiber/fiber/v3` do núcleo da lib para que apps ainda em **Fiber v2** possam usar TUDO menos o middleware HTTP (core `NewTelemetry`, runtime metrics, messaging, gRPC, DB/cache).

Causa raiz: `tracing/otel.go` importava `fiber/v3` só por 2 helpers HTTP. Isso contaminava o pacote `tracing` inteiro e, transitivamente, `messagingobs` (importa tracing), `runtime` e os interceptors gRPC — `go list -deps ./messagingobs` mostrava 13 deps de fiber.

### O que mudou
- **`tracing` agora é fiber-free.** Removido o import de `fiber/v3` (e `redaction`/`observability`, que só existiam por causa das 2 funções movidas).
- **Novo pacote `grpcmiddleware/`** (fiber-free): interceptors gRPC saíram de `middleware` (que importa fiber). HTTP (`WithTelemetry`/`EndTracingSpans`) fica onde estava (fiber, correto).
- **Novo pacote `telemetrycore/`** (fiber-free): coletor de métricas de sistema (singleton único), compartilhado por HTTP e gRPC — evita duas goroutines de coleta quando a app usa os dois transportes.

### Símbolos movidos (BREAKING — ajustar import path nos callers)
| Símbolo | Antes | Depois |
|---|---|---|
| `SetSpanAttributeForParam(c fiber.Ctx, ...)` | `tracing` (`.../v2/tracing`) | `middleware` (`.../v2/middleware`) |
| `ExtractHTTPContext(ctx, c fiber.Ctx)` | `tracing` | `middleware` |
| `WithTelemetryInterceptor` (gRPC) | `middleware.TelemetryMiddleware` | `grpcmiddleware.TelemetryMiddleware` |
| `EndTracingSpansInterceptor` (gRPC) | `middleware.TelemetryMiddleware` | `grpcmiddleware.TelemetryMiddleware` |
| `UnaryClientInterceptor` (gRPC) | `middleware.TelemetryMiddleware` | `grpcmiddleware.TelemetryMiddleware` |
| `ResolveTenantIDFromGRPC` | `middleware` (mantido lá tb.) | também em `grpcmiddleware` |
| `StopMetricsCollector` / `DefaultMetricsCollectionInterval` | `middleware` (mantidos p/ compat) | fonte agora em `telemetrycore` |

Novo construtor gRPC: `grpcmiddleware.NewTelemetryMiddleware(tl)` (mesma assinatura de `middleware.NewTelemetryMiddleware`).

### Fix externo pendente (midaz — outro repo, outro PR)
`midaz` `pkg/net/http/withBody.go:235` usa `SetSpanAttributeForParam` importando de `tracing`. Trocar para o pacote `middleware`:
`github.com/LerianStudio/lib-observability/v3/middleware.SetSpanAttributeForParam`.
(Apps que consomem os interceptors gRPC via `middleware` também precisam trocar para `grpcmiddleware`.)

### Verificação (`go list -deps`, deps de gofiber)
| Pacote | Antes | Depois |
|---|---|---|
| `tracing` | 13 | **0** |
| `messagingobs` | 13 | **0** |
| `runtime` | 0 | 0 |
| `grpcmiddleware` (gRPC) | (era `middleware`=13) | **0** |
| `telemetrycore` | — | **0** |
| `middleware` (HTTP `WithTelemetry`) | 13 | 13 (correto, inalterado) |

Comportamento 100% preservado (só MOVE código; nenhuma lógica de métrica/telemetria alterada). `go test -tags=unit ./...` PASS em todos os pacotes; golangci-lint (wsl_v5) 0 issues. Core OTel mantido em v1.44.0.

## FASE 2 — Wrappers de auto-instrumentação (a implementar)

| Feature | Métrica | Helper | Cobre |
|---|---|---|---|
| SQL | `db.client.operation.duration` (s) | `InstrumentSQLDB(*sql.DB)` (otelsql v0.43.0) | Postgres + MySQL/MariaDB (database/sql) |
| Cache | `db.client.operation.duration` (s) | `WrapRedis(client)` (redisotel v9.17.2) | Redis + Valkey (mesmo driver go-redis) |
| ~~Doc-DB (Mongo)~~ | — | — | **ADIADO** — otelmongo v2 sem release oficial (só pseudo-version que arrastaria otel core >v1.44.0); v1 deprecated. Entra quando v2 for tagueado. Ver BACKLOG. |
| Mensageria | `messaging.client.operation.duration` / `messaging.process.duration` (s) | InstrumentPublish/Consume (hand-roll s/ helpers de propagação existentes) | RabbitMQ (contrato compartilhado c/ lib-streaming p/ RedPanda) |

Guardrails (todos): unidade s, sem query text/params/PII como label (contrato §PROIBIDO), no-op safe, instrumento 1×.
Boundary: lib expõe HELPER; conexão/dbresolver fica na app/lib-commons. Para SQL, aplicar em cada *sql.DB ANTES do dbresolver.

## FORA deste PR
- Adoção no ledger + aposentar spans manuais (Fase 3, repo midaz).
- Dashboard genérico + rollout (Fase 4).
- Business metrics (skill Ring dev-sre, frente paralela).
- IBM MQ (backlog isolado). RedPanda/Kafka (lib-streaming, repo separado).

## BACKLOG (fora deste PR, rastreado)
- **Mongo helper (`mongoobs`):** implementar quando `otelmongo v2` (`go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo`) tiver release oficial que resolva p/ otel core v1.44.x. Hoje: v2 sem tag (só pseudo-version → arrastaria core p/ pré-release, viola pin); v1 v0.69.0 existe mas deprecated + driver v1. Padrão: mirror dos outros helpers, `SetMonitor` no `*options.ClientOptions` v2. Verificado no proxy Go 2026-07-22.

## Riscos registrados
- **otelmongo v2** exige mongo-driver v2 na APP — não na lib (a lib só provê o helper). Migração do driver é Fase 3.
- **gRPC naming:** mantido experimental `rpc.server.duration`+`rpc.grpc.status_code` p/ consistir c/ span; revisar em lockstep quando semconv migrar.
