# Metric Contract — lib-observability

> Fonte canônica dos nomes/unidade/buckets/labels das métricas emitidas pela lib.
> Toda métrica nativa DEVE seguir este contrato. Testes validam contra ele.
> Base: OpenTelemetry Semantic Conventions. Ver pre-dev `docs/pre-dev/observability-metrics-standardization/`.

## Princípios

1. **Unidade = segundos (`s`)** para toda duração (semconv). NUNCA milissegundos na lib.
   - Leitura em ms é responsabilidade do PAINEL: no Grafana, unidade do campo = `seconds (s)` → formata "50 ms"/"1.2 s" automaticamente. Fallback em query: `... * 1000` (fator 1000, multiplica; NÃO converte o label `le` de heatmap/bucket).
   - NÃO dual-emitir (ms+s) na lib. NÃO converter no collector. Compat legacy de dashboards antigos migra p/ unidade-do-painel.
2. **Instrumento criado UMA vez** na construção (nunca por request). Record é **no-op** quando o instrumento é nil (telemetria desabilitada) — chamável incondicionalmente, nunca panic, nunca afeta o request path.
3. **Cardinalidade é calculada, não estimada.** Cada combinação de atributos custa 1 série num counter e `explicit_boundary_count + 3` num histograma Prometheus (boundaries explícitos, bucket `+Inf` implícito, `_sum`, `_count`; hoje 14 + 3 = 17). O SDK OTel retém no máximo 2.000 attribute-sets por instrumento e colapsa o excedente em `otel.metric.overflow=true`, descartando a identidade do tenant. Portanto, o limite deve ser respeitado separadamente por instrumento. No cenário de 50 tenants, 30 rotas e 6 classes de status: request counter = 1.500 sets; error counter = até 1.500; latency histogram = 300 sets / 5.100 séries; total aproximado = 8.100 séries. Um único counter com tenant × rota × status teria 9.000 sets e já quebraria o filtro por tenant. O orçamento não é garantia universal: cada adotante deve recalcular tenants autenticados × rotas normalizadas e manter cada instrumento abaixo de 2.000 sets. Identidade entra em counter dimensionado ou trace, nunca em histograma de alta dimensão.
4. **Nomes = OTel semconv estável.** Não inventar chaves em instrumentos semconv. Quando a semconv proíbe estender uma métrica estável com identidade, criar instrumento próprio sob `lerian.` em vez de contaminar o nome estável.
5. **tenant.id** nunca entra em métricas HTTP padrão nem em métricas de infra (`sqlobs`, `redisobs`, `messagingobs`): esses recursos são compartilhados entre tenants e o label seria semanticamente errado. Para HTTP, é permitido somente nas métricas `lerian.*.by_tenant` opt-in, a partir de identidade autenticada explicitamente no contexto da aplicação; header, baggage, metadata, AttrBag e atributos genéricos de span não são fontes confiáveis e não podem fornecer nem sobrescrever o `tenant.id` autenticado.

## Buckets advisory (por sinal)

| Sinal | Buckets (segundos) |
|---|---|
| HTTP / RPC / Messaging | `0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10` |
| Database | `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10` (mais fino no low-end) |

## Catálogo (nome / tipo / unidade / labels)

| Métrica | Tipo | Unidade | Labels permitidos | Estabilidade |
|---|---|---|---|---|
| `http.server.request.duration` | Histogram | s | http.request.method, http.response.status_code, http.route, error.type | STABLE (já existe) |
| `lerian.http.server.requests.by_tenant` | Counter | {request} | tenant.id autenticado, http.route | OPT-IN |
| `lerian.http.server.errors.by_tenant` | Counter | {error} | tenant.id autenticado, http.route | OPT-IN; somente HTTP 5xx |
| `lerian.http.server.latency.by_tenant` | Histogram | s | tenant.id autenticado, http.response.status_class (1xx–5xx, other) | OPT-IN |
| `http.client.request.duration` | Histogram | s | http.request.method, http.response.status_code, server.address, error.type | STABLE (já existe) |
| `http.server.active_requests` | UpDownCounter | {request} | http.request.method, http.route (opcional) | (T4) |
| `rpc.server.duration`¹ | Histogram | s | rpc.system, rpc.method, rpc.grpc.status_code, error.type, tenant.id | (T2) |
| `rpc.client.duration`¹ | Histogram | s | rpc.system, rpc.method, rpc.grpc.status_code, error.type | (T2) |
| `db.client.operation.duration` | Histogram | s | db.system.name, db.operation.name, db.collection.name, db.namespace, error.type | STABLE (Fase 2) |
| `messaging.client.operation.duration` (produce) | Histogram | s | messaging.system, messaging.operation.name, messaging.destination.template, error.type | (Fase 2) |
| `messaging.process.duration` (consume) | Histogram | s | messaging.system, messaging.operation.name, messaging.destination.template, messaging.consumer.group.name, error.type | (Fase 2) |
| `go.*` (runtime) | Gauge/Counter/Hist | várias | (dimensões fixas do contrib/runtime) | (T3) |

¹ **Nota gRPC (validar na T2):** o train contrib atual pode emitir `rpc.server.duration` (experimental, este contrato) ou `rpc.server.call.duration` (RC, semconv). Confirmar o nome contra o pacote pinado no momento da T2 e alinhar. Manter `rpc.grpc.status_code` OU `rpc.response.status_code` conforme o que o interceptor da lib emite (hoje o span/métrica usam `rpc.grpc.status_code`, setado em `grpcmiddleware.WithTelemetryInterceptor` / `recordRPCDuration`).

## PROIBIDO como label (PII / cardinalidade ilimitada)

- query text / SQL / bind params / valores de coluna
- `db.query.text` → só em SPAN, opt-in; NUNCA em métrica
- routing key / message id / partition com id
- `url.path` com id/uuid; path resolvido concreto (span.name — raiz do problema original)
- pix key, document (cpf/cnpj), email, qualquer PII
- request/response payload

## Habilitação / no-op

- Métrica emitida quando telemetria habilitada (provider+MetricsFactory não-nil — sinal existente na lib) e o subsistema ligado.
- Toggles opt-in em `TelemetryConfig` p/ subsistemas novos (runtime, db-instrumentation), default-safe, degradam p/ no-op quando off. Nunca erro que quebre a app.

## Referências
- Padrão de implementação (template): `middleware/telemetry.go` — `newHTTPServerDurationHistogram` (:45-61), `recordHTTPServerDuration` (:386+).
- semconv: opentelemetry.io/docs/specs/semconv/{http,rpc,database,messaging}/*
- Decisões: `docs/pre-dev/observability-metrics-standardization/trd.md` (ADRs), `dependency-map.md` (versões).
