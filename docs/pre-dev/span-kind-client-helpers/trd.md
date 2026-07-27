# Gate 3 — TRD: span-kind-client-helpers

> Gates anteriores: [research.md](./research.md) · [prd.md](./prd.md) · [feature-map.md](./feature-map.md)
> Data: 2026-07-26 · Track: Full · Status: draft (aguardando aprovação)
> ⚠️ Arquitetura em PATTERNS técnico-agnósticos. Versões/pacotes concretos → Gate 6 (dependency-map).

## Metadata (Step 0)

```yaml
feature: span-kind-client-helpers
gate: 3
artifact_type: shared-library          # NÃO é serviço — biblioteca pública importável
deployment:
  model: N/A                           # lib publicada via release da própria biblioteca; sem runtime deployado
tech_stack:
  primary: Go (Backend library)
  standards_loaded: [golang.md (Ring)] # core/domain/quality/architecture aplicáveis
project_rules:
  source: CLAUDE.md + docs/metrics-contract.md   # a lib usa estes como PROJECT_RULES de fato (não há docs/PROJECT_RULES.md; convenções já canonizadas)
auth: None                             # lib de telemetria; sem lib-auth
license_manager: None                  # sem lib-license-go
project_technologies:
  - category: outbound HTTP instrumentation
    prd_requirement: F1 (classificação automática de saída HTTP + medição por dependência)
    choice: "capability: standard HTTP-client instrumentation library (transport wrapper)"  # produto concreto → Gate 6
    rationale: reusar instrumentação de mercado (não reinventar span+métrica+propagação)
  - category: span classification helper
    prd_requirement: F2 (atalho manual p/ casos sem cobertura)
    choice: "capability: existing tracing API of the library"  # sem nova dep
    rationale: só injeta a classificação correta sobre o tracer já existente
```

> **Nota sobre PROJECT_RULES:** o Step-0 do skill pede `docs/PROJECT_RULES.md`. Esta biblioteca não o tem porque suas convenções já estão canonizadas em `CLAUDE.md` (layout, commit, lint, testes, release) e `docs/metrics-contract.md` (contrato de métricas test-validado). Ambos cumprem o papel de PROJECT_RULES e são a fonte-de-verdade deste TRD. Não criar um novo arquivo redundante.

---

## 1. NFRs (atributos de qualidade)

| Atributo | Alvo | Origem |
|---|---|---|
| **Segurança de adoção** | Adotar os recursos NUNCA quebra a app: sem panic, sem travar o caminho da requisição; telemetria off → sem efeito (no-op) | PRD CA6/F5 |
| **Privacidade / cardinalidade** | Nenhum dado sensível ou identificador de alta cardinalidade vira rótulo/atributo (caminho de URL com id, dado pessoal) | PRD CA7/F5; metrics-contract.md:40-47 |
| **Conformidade de contrato** | A medição de duração por dependência HTTP conforma nome/unidade/rótulos ao contrato publicado | PRD CA2/F4; metrics-contract.md:29 |
| **Compatibilidade** | Puramente aditiva; zero mudança de comportamento existente | PRD CA8 |
| **Consistência de padrão** | Novos meios seguem o MESMO padrão dos wrappers existentes; princípio de precedência documentado e redundante | PRD CA4/CA5; feature-map Domínio B |
| **Overhead** | Instrumento criado 1× na construção (nunca por chamada); custo por chamada = o mínimo da instrumentação de mercado reusada | metrics-contract.md princípio 2 |
| **Zero-imposição de exportador** | A lib não impõe exportador; propaga contexto e respeita os provedores injetados | Ring golang.md (OTel) |

---

## 2. Estilo de arquitetura

**Modular library, flat package layout.** A biblioteca já é um conjunto de pacotes coesos no root do módulo, um por preocupação de instrumentação (rastreamento, wrappers de banco SQL, de cache, de mensageria, middleware de servidor). Esta feature **adiciona** dois pontos sem alterar o estilo:

1. um **novo pacote de instrumentação de saída HTTP** (irmão dos wrappers de banco/cache existentes);
2. um **helper de classificação de span** dentro do **domínio de rastreamento já existente** (não é pacote novo — span é concern do rastreamento).

Mais uma **camada transversal de convenção/documentação** (não é código executável — é o princípio de precedência espalhado por README, cartões de pacote e textos de ajuda).

**Pattern central: "Thin instrumentation wrapper".** Todo wrapper da biblioteca é uma casca fina sobre uma instrumentação de mercado (delegação, não reimplementação), aplicando os *defaults corporativos* (provedores, guardrails de privacidade/cardinalidade, rótulos canônicos) e degradando para no-op quando a telemetria está desligada. Os dois entregáveis seguem esse pattern: o wrapper HTTP delega a uma instrumentação HTTP-cliente padrão; o helper de span delega ao tracer existente, apenas prefixando a classificação correta.

---

## 3. Componentes

### C1 — Wrapper de instrumentação de saída HTTP (pacote novo)
- **Responsabilidade única:** transformar um "meio de transporte HTTP" comum em um instrumentado, de modo que toda chamada de saída seja classificada como "dependência externa" e produza a medição de duração por dependência.
- **Inbound (o que recebe):** um meio de transporte base (opcional; se ausente, usa o transporte padrão), e um conjunto de opções de configuração (provedores de telemetria, propagação de contexto, formatação do nome da operação, atributos extras limitados).
- **Outbound (o que produz):** um meio de transporte instrumentado, pronto para ser injetado no cliente HTTP da aplicação; e, em runtime, a classificação "dependência externa" + a medição de duração + a propagação de contexto de rastreamento nos cabeçalhos de saída.
- **Boundary:** NÃO cria nem possui o cliente HTTP da aplicação (a app monta o seu, incl. configurações de segurança de transporte, e injeta o transporte instrumentado). NÃO dispara requisições. Só envolve o transporte.
- **Guardrails:** (a) sem provedor de rastreamento injetado → não há classificação de span (degrada silencioso, ver ADR-005); (b) nome da operação **limitado por padrão** (não inclui caminho concreto); (c) rótulos restritos ao conjunto canônico (método, resultado, destino) — caminho/consulta de URL nunca viram rótulo.
- **Conformidade:** a medição emitida bate com o contrato de métricas publicado (nome/unidade/rótulos) — verificado por teste.

### C2 — Helper de classificação de span (no domínio de rastreamento existente)
- **Responsabilidade única:** dado o rastreador já existente, iniciar um span **já classificado** como "dependência externa" (a classificação certa por padrão), sem que o chamador precise conhecer os detalhes de baixo nível.
- **Inbound:** o contexto, o rastreador (obtido pelos meios já existentes da biblioteca), o nome da operação e opções de span do chamador.
- **Outbound:** o contexto derivado e o span iniciado com a classificação correta.
- **Boundary:** NÃO emite métrica, NÃO impõe contrato de privacidade/cardinalidade sobre o nome/atributos (o chamador é dono disso — documentado). É o menor átomo possível: só injeta a classificação.
- **Comportamento de precedência:** a classificação "dependência externa" é aplicada como **default sobreponível** — se o chamador informar deliberadamente outra classificação, a dele prevalece (ADR-004).
- **Uso previsto:** casos **sem cobertura automática** — em especial o banco de documentos (ADR-003) e chamadas a serviços custom.

### C3 — Camada de convenção/documentação (transversal, sem código executável)
- **Responsabilidade única:** garantir que o **princípio de precedência** seja encontrável e inequívoco para humanos e assistentes de IA.
- **Superfície:** (a) documentação de uso geral da biblioteca; (b) cartão de identidade de cada pacote de instrumentação; (c) texto de ajuda do próprio helper manual (C2).
- **Conteúdo normativo:** "prefira sempre o wrapper que existe (banco SQL, cache, HTTP, mensageria, servidor); use o helper manual só quando não houver wrapper para o caso; nunca instrumente em dobro" (ADR-006).

---

## 4. Fluxos

### Fluxo 1 — Instrumentação automática de saída HTTP (F1)
```
App monta transporte base (com sua config de segurança)
   → App envolve o transporte pelo wrapper C1 (injetando provedores de telemetria)
   → App usa o cliente HTTP com o transporte instrumentado
   → a cada chamada de saída:
        · span classificado "dependência externa" (se provedor de rastreamento injetado)
        · medição de duração por dependência (destino/método/resultado) conforme contrato
        · contexto de rastreamento propagado nos cabeçalhos
   → resposta lida/fechada pela app → span encerra
```

### Fluxo 2 — Classificação manual de saída sem cobertura (F2)
```
App obtém o rastreador pelos meios existentes da biblioteca
   → chama o helper C2 (contexto, rastreador, nome, opções)
   → span iniciado já classificado "dependência externa"
   → app executa a chamada (ex.: banco de documentos), encerra o span
```

### Fluxo 3 — Decisão orientada (F3, precedência)
```
Dev/IA vai instrumentar uma saída
   → consulta a convenção C3 (em qualquer ponto de leitura)
   → há wrapper para este tipo? SIM → usa o wrapper (C1 p/ HTTP; SQL/cache/etc. existentes)
                                 NÃO → usa o helper manual C2
   → nunca aplica wrapper + manual na mesma chamada (anti-dobro)
```

---

## 5. ADRs

**ADR-003: Banco de documentos via helper manual, não instrumentação de driver**
- **Context:** chamadas ao banco de documentos são saída de rede e devem ser "dependência externa"; falta cobertura automática para elas.
- **Options:** (a) instrumentação automática no nível do driver do banco de documentos; (b) classificar manualmente via o helper de span (C2).
- **Decision:** (b) — helper manual.
- **Rationale:** a instrumentação de driver disponível para a versão de driver em uso está imatura/instável (avaliado 2026-07-25); adotá-la agora traria risco desproporcional. O helper manual resolve a classificação imediatamente, sem nova dependência.
- **Consequences:** o banco de documentos fica coberto pelo caminho manual (não automático). **Divergência intencional** do plano de padronização de métricas anterior (que apostava em instrumentação automática de driver para esse caso). Reavaliar quando a instrumentação de driver amadurecer. Registrado como divergência controlada, não como conflito.

**ADR-004: Classificação como default sobreponível (prefixar a opção)**
- **Context:** o helper manual (C2) deve garantir a classificação "dependência externa" por padrão, mas sem impedir um chamador que precise, deliberadamente, de outra classificação.
- **Options:** (a) **forçar** a classificação (ignora qualquer opção do chamador); (b) aplicá-la como **default que o chamador pode sobrepor**.
- **Decision:** (b) — default sobreponível.
- **Rationale:** o mecanismo de opções da plataforma de rastreamento resolve por "a última vence" (confirmado por leitura do código-fonte na pesquisa). Aplicando a classificação **antes** das opções do chamador, ela vira o default e uma classificação explícita do chamador (informada depois) prevalece. Preserva a flexibilidade (PRD US7) sem sacrificar o default correto.
- **Consequences:** comportamento previsível e alinhado à semântica padrão; documentar que informar a própria classificação sobrepõe o default.

**ADR-005: Sem provedor de rastreamento injetado → sem classificação de span (degrada silencioso)**
- **Context:** a instrumentação de mercado usada pelo wrapper HTTP só cria o span classificado quando um provedor de rastreamento é fornecido; caso contrário não há span (mas pode haver medição).
- **Options:** (a) exigir provedor (falhar se ausente); (b) degradar silenciosamente para no-op de span quando ausente.
- **Decision:** (b) — degradar silencioso, **e** garantir que o caminho "telemetria ligada" da biblioteca injete explicitamente o provedor de rastreamento.
- **Rationale:** alinhado ao NFR de segurança de adoção (telemetria off nunca quebra). Mas, para a feature entregar valor quando ligada, o provedor precisa ser injetado — responsabilidade do wrapper/config, não do chamador.
- **Consequences:** documentar claramente que o span de "dependência externa" só aparece com provedor de rastreamento ativo; testes cobrem tanto o caminho on quanto o no-op.

**ADR-006: Precedência automático-vs-manual (anti-dupla-instrumentação)**
- **Context:** existindo wrapper automático (C1 e os já existentes) e helper manual (C2), há risco de o dev aplicar os dois na mesma chamada, gerando classificação/telemetria duplicada.
- **Options:** (a) só documentar as ferramentas; (b) documentar uma **regra de precedência** explícita e o alerta anti-dobro.
- **Decision:** (b) — regra de precedência normativa (C3).
- **Rationale:** a causa-raiz do problema (PRD §1) é falta de padrão claro; a ferramenta sozinha não corrige o comportamento. A regra torna o caminho certo óbvio e o errado explicitamente desaconselhado.
- **Consequences:** a documentação vira parte do entregável com critério de aceite próprio (CA4/CA5); qualquer PR que adicione um wrapper novo deve estender a convenção.

**ADR-008: Redação de `url.full` no collector, não na lib (descoberto na implementação 2026-07-27)**
- **Context:** a instrumentação HTTP-cliente de mercado (otelhttp v0.69.0) **sempre** grava `url.full` (URL crua, com path e query) como atributo do span CLIENT. Se a URL de saída carrega PII (CPF/id/Pix no path/query — confirmado pelo usuário que ocorre), isso vaza para o trace. O CA7 pedia "nunca expõe url.path/query".
- **Options:** (a) filtrar/redigir `url.full` dentro da lib; (b) redigir no OTel Collector (transform processor); (c) aceitar cru.
- **Decision:** (b) — redação no collector.
- **Rationale:** investigação empírica no SDK OTel-Go v1.44 mostrou que **não há via limpa/suportada** de remover um atributo que a instrumentação setou no span: `OnStart` (ReadWriteSpan) roda antes do otelhttp setar `url.full` e seria sobrescrito; `OnEnd` é ReadOnly; exporter-decorator exigiria reconstruir spans (pesado, acoplado a internals). A lib garante o que controla — **métrica com labels bounded/PII-free e span-name bounded** (verificado por teste). A redação de `url.full` (atributo semconv padrão) pertence ao pipeline central onde a Lerian já opera cardinalidade/redação.
- **Consequences:** `httpobs` NÃO tenta remover `url.full`. Fica **follow-up**: regra transform/redact de `url.full` no OTel Collector (repo de config do collector, NÃO a lib). Usuário indicou que provavelmente já existe transform — validar. Documentado no README (nota "PII in outbound URLs") e no doc.go do httpobs. Também: removida a Option `WithAttributes` do httpobs (otelhttp v0.69.0 não tem via genérica de atributos de transport — YAGNI).

**ADR-007: Rótulos de telemetria como valores literais canônicos (espelhar o produtor existente)**
- **Context:** a medição de duração por dependência HTTP precisa de um conjunto de rótulos canônicos; a biblioteca já tem um produtor equivalente do lado "servidor".
- **Options:** (a) centralizar as chaves de rótulo num catálogo de constantes; (b) usar os mesmos **valores literais canônicos** que o produtor "servidor" já usa.
- **Decision:** (b) — espelhar o produtor existente (valores literais), porque o catálogo de constantes atual só cobre outra família (banco de dados) e não os rótulos HTTP.
- **Rationale:** consistência imediata com o que já é emitido do lado servidor; evita divergência e um refactor fora de escopo do catálogo de constantes.
- **Consequences:** rótulos HTTP ficam alinhados servidor↔cliente; se no futuro o catálogo de constantes ganhar a família HTTP, migrar ambos juntos (follow-up, fora de escopo).

---

## 6. Segurança / Privacidade

- **Sem auth/license:** biblioteca de telemetria; não há autenticação, autorização, nem validação de licença (metadata Step 0).
- **Threat model relevante = vazamento de dado sensível via telemetria.** Mitigação (NFR privacidade): guardrail que impede caminho/consulta de URL e dado pessoal de virarem rótulo/atributo; nome de operação limitado por padrão. Verificado por teste que falha se dado proibido aparecer (espelha os testes anti-vazamento já existentes nos wrappers de banco/cache).
- **Cardinalidade como risco de custo/segurança operacional:** o rótulo de destino pode explodir se a app chama muitos destinos distintos — documentado como risco conhecido, com recomendação de normalização/filtragem no consumo (detalhe de implementação → API-design/tasks).

---

## 7. Integração

| Integração | Tipo | Pattern | Erros |
|---|---|---|---|
| Contrato de métricas publicado | interna, síncrona (conformidade estática) | wrapper conforma nome/unidade/rótulos; teste valida | divergência = falha de teste |
| Wrappers existentes (banco/cache/mensageria/servidor) | interna (consistência de padrão) | novo wrapper imita a construção dos existentes | inconsistência = revisão |
| Plano de padronização de métricas | interna (complemento) | entrega o produtor de uma medição prevista lá; não edita o plano | conflito único (banco de documentos) resolvido por ADR-003 |
| Instrumentação de mercado (HTTP-cliente) | externa (capacidade) | delegação fina (thin wrapper); provedores injetados | ausência de provedor → no-op (ADR-005) |

**Versionamento:** biblioteca versionada por release próprio (beta em canal de desenvolvimento, estável no canal principal). Feature aditiva → sem quebra de compatibilidade.

---

## 8. Deployment (lógico)

N/A — não há runtime deployado. O entregável é código de biblioteca publicado via o processo de release existente da própria biblioteca. Aplicações consumidoras absorvem a feature ao atualizar a dependência (trabalho downstream, fora de escopo).

---

## Gate 3 — Validação

- [x] Todos os domínios do feature-map mapeados a componentes (A→C1/C2, B→C3)
- [x] Todas as features do PRD mapeadas (F1→C1, F2→C2, F3→C3, F4→C1 conformidade, F5→NFR/guardrails)
- [x] Fronteiras de componente claras (responsabilidade única + boundary por componente)
- [x] Interfaces técnico-agnósticas (capacidades, sem produto/versão no corpo)
- [x] Propriedade de dados: N/A explicitado (sem persistência — §8, data-model será N/A justificado)
- [x] Atributos de qualidade endereçados (§1) e atingíveis (patterns provados, clona wrappers existentes)
- [x] Integrações identificadas por capacidade (§7); padrões, não ferramentas
- [x] ADRs registrados (003–007) sem nomes de produto
- [x] Sem auth/license (metadata); sem paginação (não há listagem)
- [x] Zero nomes de produto/versão no corpo (stack só no metadata do Step 0, como o skill permite)

**Confidence:** Pattern match 40 (existe antes — clona sqlobs/redisobs) · Complexity 30 (simples, provado) · Risk 30 (baixo, aditivo, mitigado) = **100/100 → autônomo**.

**Resultado do Gate:** ✅ PASS (sujeito à aprovação humana)
