# PRD — Fix: nomes de operação HTTP com alta cardinalidade e dado sensível

## Metadata
- **Date:** 2026-07-17
- **Feature:** fix-span-name-cardinality-pii
- **Track:** Small (4 gates)
- **Gate:** 1 (PRD)
- **Research:** [research.md](./research.md)
- **Confidence:** 90/100 (problema quantificado em produção; solução é convenção padrão de indústria; valor/ROI claros)

---

## 1. Problema

A biblioteca de observabilidade nomeia cada operação HTTP recebida usando o **caminho concreto da requisição, com os identificadores embutidos** (ex.: a chave Pix / CPF de uma consulta). Como cada requisição tem um identificador diferente, cada uma vira um "nome de operação" único. Isso gera dois danos simultâneos:

1. **Explosão de custo e degradação de observabilidade.** Nomes únicos por requisição multiplicam as séries de métricas derivadas. Em produção, **um único serviço gerou ~50.000 nomes de operação distintos — cerca de 72% de todas as séries ativas** do backend de métricas, encarecendo o armazenamento e tornando impossível agrupar/analisar operações semelhantes (todas as consultas de chave viram "operações diferentes").

2. **Vazamento de dado pessoal (LGPD).** Identificadores sensíveis — chave Pix, CPF, CNPJ, telefone — trafegam **em texto puro dentro do nome da operação** até o backend de traces, onde ficam armazenados. É exposição de dado pessoal em um local não previsto para isso.

### Evidência / impacto quantificado
- ~50.000 nomes distintos em um serviço; ~72% das séries ativas do backend de métricas (medido em produção).
- Redução comprovada de ~99% na cardinalidade desse serviço quando os identificadores deixam de compor o nome da operação (validado por mitigação temporária no pipeline central).
- Dado sensível (CPF/CNPJ/chave Pix) presente hoje nos nomes de operação armazenados no backend de traces.

### Quem é afetado
- **Times de plataforma/observabilidade (Lerian):** pagam o custo da cardinalidade e operam o backend saturado.
- **Times de produto que consomem a biblioteca:** dashboards e buscas ficam poluídos; não conseguem analisar latência/erro por rota de forma agregada.
- **Área de compliance/privacidade:** responsável pelo risco regulatório do dado pessoal exposto.
- **Clientes finais (indireto):** têm seus identificadores pessoais expostos em telemetria interna.

### Workaround atual
Uma mitigação foi aplicada **no pipeline central de telemetria** (agrega/remove o nome de operação de alta cardinalidade). Funciona para conter custo, **mas não impede o vazamento de dado pessoal** (o identificador já saiu do ambiente de origem) e precisa ser mantida serviço a serviço. Não resolve a causa raiz, que está na biblioteca.

---

## 2. Requisitos de Negócio

### Resumo executivo
Corrigir, na origem, como a biblioteca nomeia as operações HTTP: passar a usar o **padrão de rota** (o gabarito da URL, sem os identificadores concretos) em vez do caminho resolvido. Isso elimina estruturalmente o dado pessoal do nome da operação e limita a cardinalidade das métricas, alinhando-se à convenção padrão de mercado de observabilidade e removendo a necessidade de mitigações espalhadas.

### Personas
- **Engenheiro de plataforma/o11y** — *Objetivo:* manter o backend de telemetria com custo previsível e consultável. *Frustração:* um serviço domina as séries; alertas e dashboards ficam lentos/caros; precisa aplicar remendos manuais.
- **Desenvolvedor de produto (consumidor da lib)** — *Objetivo:* ver latência/erro por endpoint agregado. *Frustração:* cada requisição vira uma "operação" diferente; impossível agrupar por rota.
- **Responsável por privacidade/compliance** — *Objetivo:* garantir que dado pessoal só exista onde é previsto e protegido. *Frustração:* CPF/chave Pix aparecem crus na telemetria de traces.

### Histórias de usuário

**US-1 — Nome de operação sem dado pessoal**
> Como responsável por privacidade, quero que os nomes das operações HTTP nunca contenham identificadores pessoais (CPF, chave Pix, etc.), para que a telemetria não seja um vetor de exposição de dado pessoal.
- **Critério:** nenhum identificador concreto da requisição aparece no nome da operação; o nome usa o padrão de rota.
- **Critério:** dado pessoal continua fora do nome mesmo para rotas com parâmetros numéricos, slugs ou outros formatos (não só formatos específicos).

**US-2 — Métricas de operação agregáveis e baratas**
> Como engenheiro de plataforma, quero que operações semelhantes compartilhem o mesmo nome, para que as métricas fiquem em baixa cardinalidade e o custo/consulta do backend seja saudável.
- **Critério:** requisições para o mesmo endpoint (diferindo só nos identificadores) compartilham um único nome de operação.
- **Critério:** a cardinalidade de nomes de operação por serviço cai para a ordem do número de rotas do serviço (não do número de requisições).

**US-3 — Análise por rota preservada**
> Como desenvolvedor de produto, quero continuar filtrando e agrupando telemetria por rota, para não perder capacidade de análise que já tenho hoje.
- **Critério:** a informação de rota (padrão da URL) continua disponível para filtro/agrupamento nas métricas e nos traces.
- **Critério:** requisições sem rota reconhecida (ex.: caminho inexistente / 404) não são forçadas a um nome enganoso — o nome degrada de forma segura e não polui as séries.

**US-4 — Consistência entre sinais**
> Como engenheiro de o11y, quero que o nome da operação e a métrica de duração da requisição concordem sobre "qual é a rota", para navegar entre métrica e trace sem discrepância.
- **Critério:** o nome da operação e a métrica de duração referenciam a mesma rota para a mesma requisição.

### Métricas de sucesso
- **Cardinalidade:** redução ≥95% no número de nomes de operação distintos do serviço mais afetado (baseline: ~50k → ordem de dezenas).
- **Privacidade:** 0 identificadores pessoais detectáveis em nomes de operação novos após o rollout (amostragem no backend de traces).
- **Sem regressão de análise:** capacidade de filtrar/agrupar por rota mantida (métrica de duração por rota continua funcionando).
- **Adoção:** consumidores conseguem atualizar a biblioteca com um caminho de migração claro para o impacto de mudança de comportamento.

---

## 3. Escopo

### Dentro do escopo
- Corrigir, na biblioteca, o nome das operações HTTP recebidas para usar o padrão de rota em vez do caminho concreto.
- Garantir o comportamento seguro para requisições sem rota reconhecida (nome degradado, sem poluir séries).
- Garantir que a informação de rota permaneça disponível para análise (não remover capacidade existente).
- Fornecer um caminho de migração/comunicação para consumidores, dado que é **mudança de comportamento observável** (ver Assunções).
- Cobertura de testes que garanta o novo comportamento e previna regressão.

### Fora do escopo
- **Corrigir o dado pessoal já armazenado** no backend de traces (histórico). Esta mudança previne vazamentos **futuros**; a limpeza do histórico é um esforço separado.
- **Remover a mitigação do pipeline central** existente. Pode ser reavaliada depois que o fix estiver difundido nos consumidores — decisão separada.
- **Mudanças em como cada serviço consumidor registra/ordena seus componentes de middleware.** Tratado como feature separada (repo do consumidor). *Nota: a pesquisa indicou uma dependência de ordem que pode exigir coordenação entre a biblioteca e o consumidor — a resolução técnica dessa dependência é decidida no TRD, não aqui.*
- Escolhas de implementação (como renomear a operação com segurança, se haverá ponto de extensão para customização, alinhamento de convenções internas) — **deferidas ao TRD**.

### Assunções
- A informação de rota (padrão da URL) **já é conhecida** pela biblioteca no momento adequado (a métrica de duração já a utiliza corretamente hoje) — logo o nome da operação pode ser alinhado à mesma fonte.
- A mudança do formato do nome da operação é **breaking change** do ponto de vista de versionamento: dashboards e buscas de consumidores que agrupam pelo nome antigo (com caminho concreto) serão impactados. O rollout deve comunicar isso e seguir o processo de release da biblioteca (pré-lançamento antes de estável).

### Dependências de negócio
- Consumidores precisarão atualizar a versão da biblioteca para receber o fix; produtos regulados (fluxo Pix/BACEN) são prioridade pelo componente de privacidade.
- Comunicação com times donos de dashboards que hoje dependem do nome de operação antigo.

---

## 4. Diferenciação / Justificativa
- **Alinha ao padrão de indústria** de nomeação de operações HTTP (método + padrão de rota), que é a prática recomendada consolidada para observabilidade — reduz surpresa para novos consumidores e ferramentas.
- **Corrige na origem** em vez de remediar no pipeline: resolve custo **e** privacidade de uma vez, e para todos os consumidores da biblioteca, eliminando remendos por serviço.
- **ROI:** elimina ~72% de séries desperdiçadas no maior ofensor (custo direto de armazenamento/consulta) e remove um risco regulatório concreto (dado pessoal em telemetria).

---

## Gate 1 — Validação
| Categoria | Status |
|---|---|
| Problema articulado (1-2 frases) | ✅ |
| Impacto quantificado | ✅ (~50k nomes, ~72% séries, CPF/chave Pix expostos) |
| Usuários identificados | ✅ (plataforma, produto, compliance) |
| Features endereçam o problema | ✅ (US-1 privacidade, US-2 cardinalidade, US-3/4 análise) |
| Métricas mensuráveis | ✅ |
| Escopo in/out explícito | ✅ |
| Requisito regulatório documentado | ✅ (LGPD — dado pessoal em telemetria) |
| Decisões técnicas deferidas ao TRD | ✅ (hazard de ordem, ponto de extensão, semconv) |

**Resultado:** ✅ PASS → Gate 2 (TRD)
