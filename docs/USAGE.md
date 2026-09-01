# lib-observability — Guia de Instrumentação (v2)

> Módulo: `github.com/LerianStudio/lib-observability/v4` · Go >= 1.26.3
> Este guia é PRESCRITIVO: siga a ordem e copie os snippets. Todas as assinaturas são reais.
> Público: engenheiro (ou IA) que vai instrumentar um serviço Lerian.

---

## 0. Modelo mental (leia antes de tudo)

A lib emite **métricas OTLP** que vão: `app → OTel SDK (lib) → collector → Mimir`. Duas categorias:

1. **Automática por transporte** — você registra um middleware/interceptor OU troca o client por um instrumentado, e **cada** request/query/mensagem vira métrica. Sem código por-operação.
2. **Manual (negócio)** — você chama `Record*`/`Counter` explicitamente nos pontos de negócio.

**Unidade de toda duração = segundos** (semconv). No Grafana, marque a unidade do painel como `seconds (s)` → ele formata "50 ms"/"1.2 s" sozinho. NUNCA converta na app.

**PEGADINHA CRÍTICA (nº1 de bugs):** nada é emitido se a telemetria não estiver ligada corretamente. A regra de ouro:
- `NewTelemetry(cfg)` com `EnableTelemetry: true` + `CollectorExporterEndpoint` preenchido.
- `NewTelemetry` chama `ApplyGlobals()` internamente → registra o MeterProvider como GLOBAL. **Os helpers `sqlobs`/`redisobs` usam o provider global por padrão** (`otel.GetMeterProvider()`). Se a telemetria não for criada via `NewTelemetry` (ou `ApplyGlobals` não rodar), esses helpers rodam **sem erro e sem emitir nada** (provider no-op). Se em dúvida, passe o provider explícito com `WithMeterProvider(tel.MeterProvider)`.

---

## 1. Bootstrap (obrigatório, uma vez, no início do serviço)

> **NUNCA hard-code endpoint/URL no código.** Toda configuração vem de **variáveis de ambiente** (injetadas pelo Helm). Use os nomes canônicos já adotados nos serviços Lerian (ver `.env.example`): `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_RESOURCE_SERVICE_NAME`, `OTEL_RESOURCE_SERVICE_VERSION`, `OTEL_RESOURCE_DEPLOYMENT_ENVIRONMENT`, `OTEL_LIBRARY_NAME`, `ENABLE_TELEMETRY`, `ENV_NAME`.

```go
import (
    "context"
    "log"
    "os"
    "strconv"

    "github.com/LerianStudio/lib-observability/v4/tracing"
)

// parseBoolEnv: false quando NÃO setada; erro em valor inválido (não engole typo
// silenciosamente — "ture" não deve virar telemetria desligada sem aviso).
parseBoolEnv := func(key string) (bool, error) {
    v, ok := os.LookupEnv(key)
    if !ok {
        return false, nil // NÃO setada = default false
    }
    // setada (mesmo que vazia): valida. Valor vazio/inválido → erro visível,
    // p/ um valor vazio do Helm não desligar telemetria silenciosamente.
    return strconv.ParseBool(v)
}

enableTel, err := parseBoolEnv("ENABLE_TELEMETRY")
if err != nil { log.Fatalf("ENABLE_TELEMETRY inválido: %v", err) }
insecure, err := parseBoolEnv("OTEL_EXPORTER_OTLP_INSECURE")
if err != nil { log.Fatalf("OTEL_EXPORTER_OTLP_INSECURE inválido: %v", err) }

// DeploymentEnv: prefere OTEL_RESOURCE_DEPLOYMENT_ENVIRONMENT; cai p/ ENV_NAME
// se a primeira não estiver setada (ambos são usados nos .env dos serviços).
deploymentEnv := os.Getenv("OTEL_RESOURCE_DEPLOYMENT_ENVIRONMENT")
if deploymentEnv == "" {
    deploymentEnv = os.Getenv("ENV_NAME")
}

tel, err := tracing.NewTelemetry(tracing.TelemetryConfig{
    LibraryName:               os.Getenv("OTEL_LIBRARY_NAME"),
    ServiceName:               os.Getenv("OTEL_RESOURCE_SERVICE_NAME"),
    ServiceVersion:            os.Getenv("OTEL_RESOURCE_SERVICE_VERSION"),
    DeploymentEnv:             deploymentEnv,
    CollectorExporterEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), // do Helm — NUNCA literal
    EnableTelemetry:           enableTel,
    EnableRuntimeMetrics:      true, // liga go.* (goroutines/heap/gc). opt-in.
    InsecureExporter:          insecure,
})
if err != nil {
    // NewTelemetry pode retornar handle nil em falha — trate e SAIA aqui,
    // NÃO siga para o defer (deferir shutdown de um tel nil causa panic).
    log.Fatalf("telemetry init: %v", err)
}
ctx := context.Background() // ctx de shutdown
defer tel.ShutdownTelemetryWithContext(ctx) // flush/close no shutdown (ou tel.ShutdownTelemetry() sem ctx)
```

- Endpoint, service name, env etc. vêm SEMPRE de env (Helm). O `.env.example` do serviço documenta os valores por ambiente. O código só lê `os.Getenv(...)`.
- `NewTelemetry` já registra os providers globais (ApplyGlobals). Não precisa chamar de novo.
- **Segurança do exporter:** em ambiente `production`/`prd`, `InsecureExporter: true` faz o `NewTelemetry` **retornar erro** (o serviço não sobe) a menos que a env `ALLOW_INSECURE_OTEL="<justificativa>"` esteja definida. Em produção o `OTEL_EXPORTER_OTLP_ENDPOINT` deve ser `https://...` e `InsecureExporter` false. Insecure só em `development`/`local` (cluster interno sem TLS). Como isso vem de env, é o Helm de cada ambiente que decide — o código não fixa nada.
- `EnableTelemetry: false` (env `ENABLE_TELEMETRY=false`) → telemetria no-op segura (nada quebra, nada emite). Padrão em dev/teste.
- `EnableRuntimeMetrics: true` → emite `go.*` automaticamente (sem mais código).

---

## 2. HTTP server (Fiber v3 APENAS) — `middleware`

> ⚠️ O middleware HTTP exige **Fiber v3**. Se o app está em Fiber v2, PULE esta seção (o resto da lib funciona sem migrar Fiber). O midaz hoje não tem essa métrica; ganha ao migrar.

Emite: `http.server.request.duration` (s) e `http.server.active_requests`.

Logs de acesso, nome do span SERVER e `url.path` usam somente o template da rota resolvido depois do handler (ex.: `/v1/accounts/:id`). Query string e valores nunca são retidos. Requests sem rota usam `/{unmatched}`; `http.route` fica ausente. `X-Tenant-Id` não entra em logs, spans nem labels das métricas HTTP.

```go
import "github.com/LerianStudio/lib-observability/v4/middleware"

tm := middleware.NewTelemetryMiddleware(tel)
app.Use(tm.WithTelemetry(tel))            // registra a instrumentação HTTP
// opcional: app.Use(tm.EndTracingSpans)  // se usar o par end-spans
```

Para métricas HTTP opt-in por tenant, use a identidade já validada no JWT. Não
registre `WithTelemetry` junto com `WithAuthenticatedTenantHTTPMetrics`, pois o
segundo já inclui a métrica HTTP padrão:

```go
tm := middleware.NewTelemetryMiddleware(tel)
app.Use(tm.WithAuthenticatedTenantHTTPMetrics(tel))
app.Use(func(c fiber.Ctx) error {
    claims := claimsFromValidatedCredential(c)
    tenantID, err := uuid.Parse(claims.TenantID) // tenantId do JWT
    if err != nil {
        return fiber.ErrUnauthorized
    }

    c.SetContext(observability.ContextWithAuthenticatedTenant(
        c.Context(), tenantID, claims.TenantSlug, // tenantSlug do JWT
    ))
    return c.Next()
})
```

Agregue sempre por `tenant_id`. Use `tenant_name` apenas como legenda
(`{{tenant_name}}`) e nunca como chave isolada: o slug pode mudar ou ser
reutilizado. Um nome ausente é omitido sem impedir a emissão da métrica. Cada
nome anterior pode permanecer no estado cumulativo do SDK durante a vida do
processo; inclua esse histórico ao dimensionar `WithMetricCardinalityLimit`.

Para anexar atributo de parâmetro a um span HTTP (ex.: entity id) SEM PII:
```go
middleware.SetSpanAttributeForParam(c, "account_id", id, "account") // c é fiber.Ctx (v3)
```

---

## 3. gRPC — `grpcmiddleware` (NÃO exige Fiber)

Emite: `rpc.server.duration` / `rpc.client.duration` (s). Labels: rpc.system=grpc, rpc.method, rpc.grpc.status_code, error.type, tenant.id (server).

```go
import "github.com/LerianStudio/lib-observability/v4/grpcmiddleware"

gm := grpcmiddleware.NewTelemetryMiddleware(tel)

// SERVER
server := grpc.NewServer(
    grpc.ChainUnaryInterceptor(gm.WithTelemetryInterceptor(tel)),
)

// CLIENT
conn, _ := grpc.NewClient(target,
    grpc.WithChainUnaryInterceptor(gm.UnaryClientInterceptor(tel)),
)
```

`tenant.id` no server gRPC é resolvido automaticamente da metadata `tenant-id`.

---

## 4. Banco SQL (Postgres, MySQL/MariaDB) — `sqlobs` (NÃO exige Fiber)

Emite: `db.client.operation.duration` (s). Labels: db.system.name, db.operation.name, db.collection.name, error.type. **Nunca** query text/params.

**Recomendado — `Setup` (uma chamada, defaults completos).** Compõe `InstrumentDB` + `RegisterDBStatsMetrics` e cuida das regras de posse do handle, que são a parte fácil de errar:

```go
import "github.com/LerianStudio/lib-observability/v4/sqlobs"

db, cleanup, err := sqlobs.Setup(rawDB, sqlobs.SystemPostgreSQL, sqlobs.WithDSN(dsn))
if db == nil {
    return err // só com rawDB nil (ErrNilDB): não há handle para seguir — abortar a inicialização
}
if err != nil { /* logar e seguir: instrumentação nunca quebra a conexão */ }
defer cleanup()   // libera as gauges de pool; idempotente
defer db.Close()  // o handle retornado tem pool PRÓPRIO — fechá-lo é do chamador

db.SetMaxOpenConns(25) // tuning DEPOIS do Setup, no handle retornado
```

**Na troca bem-sucedida** `Setup` **fecha o `rawDB`** (o handle retornado tem pool próprio — `sql.Open` é lazy, então nada foi discado). Fechar o handle retornado continua sendo do chamador: `cleanup()` só libera as gauges, não o pool. Com o DSN em mãos, `sqlobs.SetupOpen(driverName, dsn, system, opts...)` já abre instrumentado e dispensa o handle descartável.

**`WithDSN` é obrigatório** e agora é imposto: um `*sql.DB` não expõe o DSN com que foi aberto, então sem DSN (omitido ou vazio) o `Setup` não tem como recriar com segurança a configuração de conexão original — o substituto de DSN vazio pode falhar na primeira query ou conectar em defaults não intencionais do driver. Nesse caso `Setup` **recusa a troca** — devolve o `rawDB` intacto (ainda aberto e NÃO fechado), um `cleanup` no-op e `sqlobs.ErrDSNRequired`. Instrumentação nunca custa conectividade (ADR-008).

Em erro o `cleanup` nunca é nil e o handle devolvido segue usável — a única exceção é `rawDB == nil`, que devolve `nil` e `sqlobs.ErrNilDB`, porque não havia handle para preservar.

As seções abaixo mostram os passos que o `Setup` compõe (úteis p/ o caso do dbresolver, onde cada pool é instrumentado separadamente).

### 4a. Caso simples (um `*sql.DB`)
```go
import "github.com/LerianStudio/lib-observability/v4/sqlobs"

instrumented, err := sqlobs.InstrumentDB(rawDB, sqlobs.SystemPostgreSQL,
    sqlobs.WithDSN(dsn), // necessário: *sql.DB não expõe o DSN
)
if err != nil { /* tratar */ }
```

> ⚠️ **`InstrumentDB` devolve um `*sql.DB` NOVO com pool SEPARADO.** Você DEVE:
> 1. usar SÓ o `instrumented` daqui pra frente e `rawDB.Close()` no original;
> 2. reaplicar limites de pool no retornado: `instrumented.SetMaxOpenConns(...)`, `SetMaxIdleConns(...)`, `SetConnMaxLifetime(...)` (não são herdados).
>
> Alternativa: use `sqlobs.Open(driverName, dsn, system, opts...)` que já abre instrumentado.

Para MySQL/MariaDB: troque `sqlobs.SystemPostgreSQL` por `sqlobs.SystemMySQL`.

### 4b. Com dbresolver (read/write split — caso do midaz/lib-commons)
Instrumente **cada** `*sql.DB` ANTES de montar o resolver (o resolver não é instrumentável):
```go
primary, _   := sqlobs.InstrumentDB(rawPrimary, sqlobs.SystemPostgreSQL, sqlobs.WithDSN(dsnP), sqlobs.WithPoolRole(sqlobs.PoolRolePrimary))
replica, _   := sqlobs.InstrumentDB(rawReplica, sqlobs.SystemPostgreSQL, sqlobs.WithDSN(dsnR), sqlobs.WithPoolRole(sqlobs.PoolRoleReplica))
resolver := dbresolver.New(dbresolver.WithPrimaryDBs(primary), dbresolver.WithReplicaDBs(replica), dbresolver.WithLoadBalancer(dbresolver.RoundRobinLB))
```
`WithPoolRole` adiciona a label `primary|replica` (baixa cardinalidade) → visibilidade read vs write.

### 4c. Métricas de pool (opcional, alto valor p/ saturação)
> Já incluído no `Setup` — esta forma é para quem monta os passos na mão (ex.: dbresolver).
```go
reg, err := sqlobs.RegisterDBStatsMetrics(instrumented, sqlobs.SystemPostgreSQL)
if err != nil { /* tratar */ }
defer reg.Unregister() // só após sucesso
```

### 4d. APOSENTAR spans manuais (o ganho de cardinalidade)
Ao adotar `sqlobs`, **REMOVA** os `tracer.Start(ctx, "postgres.*")` manuais das camadas de repositório. Eles são a fonte de cardinalidade que o wrapper substitui.

---

## 5. Cache (Redis, Valkey) — `redisobs` (NÃO exige Fiber)

Valkey usa o mesmo driver go-redis → coberto. Emite `db.client.operation.duration`, db.system=redis.

```go
import "github.com/LerianStudio/lib-observability/v4/redisobs"

rdb := redis.NewUniversalClient(opts) // seu client go-redis v9
if err := redisobs.Instrument(rdb); err != nil { /* tratar */ }
// use rdb normalmente daqui pra frente
```
`Instrument` aplica tracing + metrics no client (in-place). Remova os spans manuais `redis.*`.

---

## 6. Mensageria (RabbitMQ) — `messagingobs` (NÃO exige Fiber)

Emite `messaging.client.operation.duration` (produce) e `messaging.process.duration` (consume). **Recebe o `*tracing.Telemetry` direto** (não usa provider global).

### 6a. Producer
```go
import "github.com/LerianStudio/lib-observability/v4/messagingobs"

pub := messagingobs.NewPublisher(tel)

ctx, headers, finish := pub.Produce(ctx, messagingobs.ProduceParams{
    DestinationTemplate: "transactions.{tenant}", // LOW-CARDINALITY. NUNCA a queue/routing key concreta
    OperationName:       "publish",
    RoutingKey:          rk,   // p/ seu log/span; NÃO vira label
    MessageID:           mid,  // idem
})
// injete `headers` na publicação AMQP (propaga o trace):
err := channel.PublishWithContext(ctx, exchange, rk, false, false, amqp.Publishing{ Headers: headers, Body: body })
finish(err) // registra a duração + error.type
```

### 6b. Consumer
```go
con := messagingobs.NewConsumer(tel)

ctx, finish := con.Consume(ctx, messagingobs.ConsumeParams{
    Headers:             delivery.Headers, // extrai o trace propagado
    DestinationTemplate: "transactions.{tenant}",
    OperationName:       "process",
    ConsumerGroup:       "ledger-workers",
})
err := handleMessage(ctx, delivery.Body)
finish(err)
```

RedPanda/Kafka: NÃO usa este pacote (vem da lib-streaming, mesmo contrato de nomes).

---

## 6.5 HTTP client / chamadas externas — `httpobs` (NÃO exige Fiber)

Para chamadas HTTP de SAÍDA (identity provider, BACEN/SPB, tenant-manager, outro serviço). Classifica como span **CLIENT** e emite `http.client.request.duration` (s). Thin wrapper sobre otelhttp.

> ⚠️ **Depende dos providers configurados** (mesma pegadinha do §0): por padrão usa os providers GLOBAIS (`otel.GetMeterProvider`/`GetTracerProvider`). Sem `MeterProvider` a métrica é no-op; sem `TracerProvider` o span não é criado. Com `NewTelemetry`+`ApplyGlobals` no bootstrap (fluxo normal) funciona; se não, passe explícito via `WithMeterProvider(tel.MeterProvider)`/`WithTracerProvider(tel.TracerProvider)`. Telemetria ausente NÃO é falha de instrumentação — é provider não configurado.

**Boundary:** a lib NÃO gerencia o lifecycle do client da app — ela envolve o **transport** que você passa (preservando TLS/timeout/proxy) e nunca faz dial/close por conta própria. `NewTransport` só embrulha um `http.RoundTripper`; `NewClient` é um **construtor de conveniência** que devolve um `*http.Client` já com o transport envolvido (passe seu base transport p/ preservar o TLS).

```go
import "github.com/LerianStudio/lib-observability/v4/httpobs"

// Opção A: envolver o transport (preserva TLS/timeout do base)
client := &http.Client{
    Transport: httpobs.NewTransport(http.DefaultTransport), // ou seu transport custom
    Timeout:   10 * time.Second,
}

// Opção B: conveniência — devolve um *http.Client já com o transport envolvido
client := httpobs.NewClient(baseTransport)
```
- Labels: `http.request.method`, `http.response.status_code`, `server.address`, `error.type`. Nome do span bounded ("HTTP GET") — nunca URL/path.
- O caller DEVE ler e fechar o response body (o span fecha no close/EOF do body).
- Opções: `WithMeterProvider`, `WithTracerProvider`, `WithPropagators`, `WithSpanNameFormatter`.

## 6.6 Saída sem wrapper (último recurso) — `tracing.StartClientSpan`

Só para saídas que NÃO têm wrapper dedicado (ex.: MongoDB — sem otelmongo v2 estável; ou uma SDK/RPC custom). Marca o span como **CLIENT** sem você precisar lembrar do `trace.WithSpanKind`.

```go
import "github.com/LerianStudio/lib-observability/v4/tracing"

ctx, span := tracing.StartClientSpan(ctx, tracer, "mongodb.find_holder")
defer span.End()
// ... a chamada externa ...
```
- **Só seta o span kind.** NÃO emite métrica e NÃO impõe contrato de PII — VOCÊ garante nome low-cardinality e sem PII.

### ⚠️ Precedência (ADR-006) — não duplicar instrumentação
Ordem de preferência por tipo de saída:

| Saída | Use |
|---|---|
| SQL | `sqlobs` |
| Redis/Valkey | `redisobs` |
| **HTTP client** | **`httpobs`** |
| mensageria | `messagingobs` |
| server (entrada) | `middleware` / `grpcmiddleware` |
| resto sem wrapper (Mongo, RPC custom) | `tracing.StartClientSpan` |

**NUNCA** envolva a mesma chamada com um wrapper (ex: httpobs) **E** abra um `StartClientSpan` em volta — isso instrumenta a operação duas vezes.

---

## 7. Métricas de NEGÓCIO (manual, opt-in) — `metrics`

A lib tem uma factory + helpers de domínio. NÃO são automáticas — você chama nos pontos de negócio.

```go
import (
    "github.com/LerianStudio/lib-observability/v4/observability"
    "github.com/LerianStudio/lib-observability/v4/metrics"
    "go.opentelemetry.io/otel/attribute"
)

// NewTrackingFromContext retorna 4 valores; a factory é o 4º:
_, _, _, f := observability.NewTrackingFromContext(ctx) // f é *metrics.MetricsFactory
// (alternativa: usar tel.MetricsFactory direto, se você tem o *tracing.Telemetry à mão)

// helper de domínio pronto (recebe atributos variádicos, NÃO orgID/ledgerID posicionais):
_ = f.RecordTransactionProcessed(ctx,
    attribute.String("organization_id", orgID),
    attribute.String("ledger_id", ledgerID),
    attribute.String("tenant.id", tenantID),
)

// custom:
c, err := f.Counter(metrics.Metric{Name: "settlements_completed", Unit: "1"})
if err != nil { /* tratar */ }
_ = c.WithAttributes(attribute.String("tenant.id", tenantID)).AddOne(ctx)
```
> ⚠️ Métrica de negócio NÃO herda `tenant.id` automático — passe explícito via `.WithAttributes` (ou nos atributos variádicos dos helpers `Record*`).

---

## 8. Checklist de adoção (para a IA seguir por serviço)

1. [ ] `NewTelemetry(...)` no bootstrap com `EnableTelemetry: true`, endpoint do collector, `EnableRuntimeMetrics: true`. `defer tel.Shutdown`.
2. [ ] gRPC: registrar `grpcmiddleware` server + client interceptors.
3. [ ] SQL: `sqlobs.InstrumentDB` em cada `*sql.DB` (antes do dbresolver), fechar o original, reaplicar pool limits. Remover spans `postgres.*` manuais.
4. [ ] Redis/Valkey: `redisobs.Instrument(client)`. Remover spans `redis.*` manuais.
5. [ ] RabbitMQ: envolver produce/consume com `messagingobs`. Remover spans de fila manuais.
6. [ ] HTTP client (saídas): usar `httpobs.NewTransport/NewClient` no `*http.Client` de chamadas externas. Remover spans de saída HTTP manuais.
7. [ ] Saídas sem wrapper (Mongo, RPC custom): trocar `tracer.Start(...)` por `tracing.StartClientSpan(...)`. NÃO duplicar com outro wrapper.
8. [ ] HTTP server (Fiber v3): `tm := middleware.NewTelemetryMiddleware(tel)` + `app.Use(tm.WithTelemetry(tel))`.
9. [ ] Negócio: garantir `Record*`/Counter nos pontos-chave (tenant.id explícito).
10. [ ] Validar no Grafana/Mimir: `db.client.operation.duration`, `rpc.*.duration`, `messaging.*.duration`, `http.client.request.duration`, `http.server.request.duration`, `go.*` aparecem para o `service.name` do serviço.

## 9. Regras invioláveis (cardinalidade / PII)
- Unidade sempre segundos. Nunca ms na app.
- NUNCA como label: query text, SQL, params, routing key, message id, url.path com id, uuid, cpf/cnpj, pix key, email, payload.
- `tenant.id`: nunca em métricas HTTP; automático em gRPC server; manual em negócio.
- Ao adotar um wrapper de infra, REMOVER o span manual equivalente (senão duplica custo).

## 10. O que NÃO está disponível ainda
- **MongoDB wrapper dedicado** (`mongoobs` estilo sqlobs/redisobs): adiado — otelmongo v2 sem release estável. **Enquanto isso**, instrumente saídas Mongo com `tracing.StartClientSpan` (§6.6) — span CLIENT correto, sem métrica automática.
- **HTTP server**: exige Fiber v3 (§2). Apps ainda em Fiber v2 não têm a métrica HTTP server nativa até migrarem; todo o resto (DB/cache/fila/HTTP client/gRPC/runtime) funciona independente do Fiber.
