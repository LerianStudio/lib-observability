# Gate 2 — Feature Map: span-kind-client-helpers

> Gates anteriores: [research.md](./research.md) · [prd.md](./prd.md)
> Data: 2026-07-26 · Track: Full · Status: draft (aguardando aprovação)
> ⚠️ Nível de negócio/domínio — sem tecnologia, componentes ou arquitetura (isso é TRD).

---

## Overview

Esta feature adiciona à biblioteca de observabilidade compartilhada os meios para **classificar corretamente chamadas de saída** como "chamada a dependência externa". São duas capacidades de instrumentação (uma automática, uma manual) unidas por uma capacidade transversal de **orientação** (o princípio de precedência). Referência: PRD Gate 1.

---

## Feature Inventory

### Core (obrigatório para o valor mínimo)

| ID | Nome | Descrição | Valor ao usuário | Depende de |
|---|---|---|---|---|
| **F1** | Classificação automática de saída HTTP | Ao usar a biblioteca para chamadas de saída via HTTP, a chamada é automaticamente marcada como "dependência externa" e passa a ter medição de duração por dependência (destino/método/resultado) | SRE enxerga latência/erro por dependência; engenheiro não erra por omissão (US1, US2) | Contrato de métricas existente (IP1); padrão dos wrappers existentes (IP2) |
| **F2** | Atalho manual para casos sem cobertura | Um único atalho de baixo esforço que marca corretamente uma chamada de saída quando **não há** cobertura automática (ex.: banco de documentos, serviço custom); default correto, com possibilidade de sobreposição deliberada | Engenheiro instrumenta o caso "sem wrapper" sem conhecer detalhes de baixo nível nem errar a classificação (US3, US7) | Domínio de rastreamento existente |

### Supporting (habilita e sustenta o valor Core)

| ID | Nome | Descrição | Valor ao usuário | Depende de |
|---|---|---|---|---|
| **F3** | Princípio de precedência (orientação humano+IA) | Regra explícita e **redundante** em todos os pontos de contato: "use sempre a cobertura automática quando existir; recorra ao atalho manual só quando não houver cobertura". Inclui alerta contra instrumentar em dobro | O jeito certo é óbvio na primeira leitura; o padrão não se degrada com o tempo (US4) | F1 e F2 (descreve como escolher entre eles) |

### Enhancement / Integration

| ID | Nome | Descrição | Valor ao usuário | Depende de |
|---|---|---|---|---|
| **F4** | Conformidade com o contrato de métricas | A medição de duração por dependência (de F1) preenche uma medição **já prevista** no contrato da biblioteca que hoje não tem produtor | Padronização: a medição sai com nome/rótulos/unidade canônicos, servindo dashboards genéricos | F1; contrato existente (IP1) |
| **F5** | Garantias transversais (segurança de adoção) | Degradação sem efeito quando telemetria off; nunca expõe dado sensível/alta-cardinalidade; puramente aditiva | Adoção sem risco de quebra nem de vazamento (US5, CA6/CA7/CA8) | F1, F2 |

> Não há features de "Integration com sistema externo" no sentido do template — as integrações aqui são **internas à biblioteca** (contrato e wrappers existentes), mapeadas como pontos de integração abaixo.

---

## Domain Groupings

### Domínio A — Instrumentação de Saída (o produto novo)
- **Propósito:** garantir que toda chamada de saída seja classificável como "dependência externa" da forma correta.
- **Features:** F1 (automática HTTP), F2 (atalho manual).
- **Fronteiras:**
  - **Owns:** a nova capacidade de marcar saída como "dependência externa" (automática e manual) e a medição de duração por dependência HTTP.
  - **Consumes:** o contrato de métricas existente (para conformar a medição); o padrão de construção dos wrappers já existentes (para consistência).
  - **Provides:** classificação correta + medição por dependência, para consumo por SRE (dashboards) e pelo time de observabilidade (destrava descarte do "interno").
- **Pontos de integração:** → contrato de métricas (IP1, conforma-se); → wrappers existentes (IP2, imita padrão); → plano de padronização de métricas (IP3, complementa).

### Domínio B — Orientação & Convenção (transversal)
- **Propósito:** tornar o uso correto óbvio e duradouro, para humanos e assistentes de IA.
- **Features:** F3 (princípio de precedência), e a face documental de F5 (avisos de segurança de adoção).
- **Fronteiras:**
  - **Owns:** a regra de precedência e os alertas anti-erro (dobro, vazamento).
  - **Consumes:** o inventário de meios de instrumentação (os wrappers automáticos existentes + F1 + o atalho manual F2) — precisa referenciá-los para dizer "prefira estes".
  - **Provides:** decisão orientada ("qual caminho usar") em todos os pontos de contato de leitura.
- **Pontos de integração:** ← Domínio A (descreve como escolher entre F1/F2 e os wrappers existentes).

> **Por que dois domínios:** F1/F2 entregam *capacidade*; F3 entrega *que o padrão seja seguido*. São valores distintos (o usuário enfatizou F3 como primeira classe). Separá-los evita que a orientação seja tratada como "documentação acessória" e garante critério de aceite próprio (CA4/CA5).

---

## User Journeys

### Jornada 1 — Engenheiro instrumenta uma chamada de saída (caminho feliz)
- **Usuário:** engenheiro de backend. **Objetivo:** dar visibilidade a uma chamada de saída.
- **Passo 1:** consulta a orientação (F3) → "existe cobertura automática para este tipo de chamada?"
- **Passo 2a (tem cobertura — ex.: HTTP):** usa a capacidade automática (F1) → chamada marcada como dependência externa + medição emitida (F4). **Sucesso.**
- **Passo 2b (não tem cobertura — ex.: banco de documentos):** usa o atalho manual (F2) → chamada marcada corretamente. **Sucesso.**
- **Falha evitada:** sem a orientação (F3), o engenheiro instrumentaria à mão mesmo onde há cobertura, ou classificaria errado → é exatamente o estado atual (1.052 vs 111).
- **Interações cross-domínio:** B (orienta) → A (executa).

### Jornada 2 — SRE diagnostica incidente de dependência
- **Usuário:** SRE em plantão. **Objetivo:** descobrir se a lentidão vem de uma dependência externa.
- **Passo 1:** abre a medição de duração por dependência (F1/F4).
- **Passo 2:** identifica qual dependência externa (identidade / Banco Central / tenants) está lenta ou com erro. **Sucesso.**
- **Falha hoje (sem a feature):** a informação não existe — a chamada está afogada em "operação interna". Diagnóstico impossível por essa via.

### Jornada 3 — Time de observabilidade destrava a redução de custo
- **Usuário:** time de observabilidade. **Objetivo:** descartar "operação interna" com segurança.
- **Passo 1:** confirma que os serviços adotaram F1/F2 → categoria "interna" deixou de conter chamadas externas.
- **Passo 2:** habilita o descarte do "interno" (trabalho downstream, fora do escopo). **Sucesso condicionado à adoção.**
- **Interação:** depende da adoção (fora do escopo desta feature), consome o resultado do Domínio A.

### Jornada 4 — Leitor futuro (humano ou IA) aprende o padrão
- **Usuário:** qualquer pessoa/IA lendo o código ou docs. **Objetivo:** aplicar o padrão certo.
- **Passo 1:** encontra a regra de precedência (F3) em qualquer ponto de entrada (visão geral da biblioteca, cartão do pacote, ou ajuda do atalho manual) — a regra é redundante, então não há caminho de leitura que a perca.
- **Passo 2:** aplica: automático quando há cobertura, manual quando não há; não instrumenta em dobro. **Sucesso.**

---

## Feature Interaction Map

```
                 ┌─────────────────────────────────────────────┐
                 │  Domínio B — Orientação & Convenção          │
                 │  F3 princípio de precedência (+ avisos F5)   │
                 │  "automático quando existe; manual quando    │
                 │   não; nunca em dobro"                       │
                 └───────────────┬─────────────────────────────┘
                                 │ orienta a escolha
                                 ▼
     ┌───────────────────────────────────────────────────────────────┐
     │  Domínio A — Instrumentação de Saída                          │
     │                                                               │
     │   F1 automática HTTP ──produz──► F4 conformidade contrato     │
     │   F2 atalho manual (sem cobertura)                            │
     │   F5 garantias transversais (off = sem efeito; sem PII)       │
     └───────┬───────────────────────────┬──────────────────────────┘
             │ conforma-se                │ imita padrão / complementa
             ▼                            ▼
      IP1 contrato de métricas    IP2 wrappers existentes (SQL/cache/
      (medição já prevista,           messaging/servidor)  ·  IP3 plano
       sem produtor → F1 é o          de padronização de métricas
       produtor)                      (complementa, não edita)
```

### Matriz de dependências

| Feature | Depende de | Bloqueia | Opcional? |
|---|---|---|---|
| F1 automática HTTP | IP1 (contrato), IP2 (padrão) | F4; valor de SRE (Jornada 2); adoção (Jornada 3) | Não (Core) |
| F2 atalho manual | domínio de rastreamento existente | caso "sem cobertura" (banco de documentos) | Não (Core) |
| F3 precedência | F1, F2 (referencia ambos) | consistência do padrão (Jornada 1 e 4) | Não (Supporting, mas é primeira classe por decisão do usuário) |
| F4 conformidade contrato | F1, IP1 | dashboards genéricos padronizados | Não |
| F5 garantias transversais | F1, F2 | adoção sem risco | Não |

Sem dependências circulares. B depende de A (para referenciá-lo); A não depende de B.

---

## Phasing Strategy

> A feature é pequena e coesa; o faseamento é lógico (ordem de entrega), não cronograma longo.

### Fase 1 — Atalho manual + orientação (fundação)
- **Features:** F2, e a parte de F3 que cobre o caminho manual; F5 (garantias).
- **Valor:** já cobre o caso sem-cobertura (banco de documentos) e estabelece o padrão. É o menor incremento que entrega valor e não depende de nova dependência externa.
- **Critério de sucesso:** atalho manual marca corretamente; regra de precedência presente; degrada seguro.
- **Gatilho p/ Fase 2:** fundação aceita.

### Fase 2 — Automática HTTP + conformidade de contrato
- **Features:** F1, F4, e a parte de F3/documentação específica de HTTP (incl. alerta anti-dobro).
- **Valor:** liga a visibilidade de dependências HTTP (identidade/Banco Central/tenants) e preenche a medição prevista no contrato.
- **Critério de sucesso:** medição de duração por dependência emitida conforme contrato; classificação correta; sem PII/alta-cardinalidade.

### Fase 3 (fora do escopo, registrado) — Adoção nas aplicações + follow-up RPC
- Migração das aplicações e classificação automática de saída via RPC — trabalho downstream/separado.

---

## Scope Boundaries

### Em escopo
- F1, F2, F3, F4, F5 (as capacidades e a orientação).

### Fora de escopo (com justificativa)
- **Adoção nas aplicações:** entrega-se o meio; adoção é downstream.
- **Cobertura automática para banco de documentos:** inviável agora (imaturidade da instrumentação disponível) → caminho é F2 (manual). Divergência intencional do plano de padronização anterior — vira ADR no TRD.
- **Classificação automática de saída via RPC:** follow-up.
- **Editar/substituir o plano de padronização de métricas:** esta feature **complementa** (entrega o produtor de uma medição prevista lá).
- **Regras de descarte no coletor:** downstream, do time de observabilidade.

### Premissas
- O contrato de métricas já prevê a medição de duração por dependência (sem produtor hoje).
- Já há cobertura automática para banco SQL e cache; esta feature segue o mesmo padrão.

### Restrições
- Puramente aditiva; nenhuma aplicação obrigada a adotar de imediato.
- Guardrail de privacidade/cardinalidade é inegociável (F5).

---

## Risk Assessment

### Riscos de complexidade de feature

| Feature | Risco | Mitigação (nível de negócio) |
|---|---|---|
| F1 automática HTTP | Medição por dependência pode gerar rótulo de alta cardinalidade (muitos destinos distintos) → custo | Tratar como risco conhecido; garantia F5 (guardrail) cobre; detalhe de mitigação vai ao TRD |
| F2 atalho manual | Uso indevido: aplicar o manual onde há cobertura automática (instrumentação em dobro) | F3 orienta explicitamente + CA5 (alerta anti-dobro) |
| F3 precedência | Orientação existir só num lugar e ser perdida na leitura | CA4 exige redundância em 3 pontos de contato |

### Riscos de integração

| Integração | Risco | Mitigação |
|---|---|---|
| IP1 contrato de métricas | Medição emitida divergir do contrato (nome/rótulo/unidade) | F4 = conformidade explícita; verificável por teste |
| IP2 wrappers existentes | Novo caminho divergir do padrão já estabelecido → inconsistência para o dev | Imitar o padrão dos wrappers existentes (decisão de research) |
| IP3 plano de padronização | Sobreposição/conflito com o plano anterior | Escopo define "complementa, não edita"; único conflito (banco de documentos) resolvido por ADR |

---

## Gate 2 — Validação

- [x] Todas as features do PRD mapeadas (F1–F5 cobrem US1–US7, CA1–CA8)
- [x] Categorias atribuídas (Core/Supporting/Enhancement)
- [x] Domínios coesos por capacidade de negócio (A instrumentação, B orientação), fronteiras claras
- [x] Jornadas completas com caminho feliz e falha evitada (4 jornadas)
- [x] Pontos de integração identificados (IP1 contrato, IP2 wrappers, IP3 plano) com direção
- [x] Sem dependências circulares (B→A apenas)
- [x] Faseamento com valor incremental (Fase 1 manual+orientação; Fase 2 HTTP+contrato)
- [x] Sem tecnologia/componentes/arquitetura no corpo

**Confidence:** Feature coverage 25 · Relationship clarity 25 · Domain cohesion 25 · Journey completeness 25 = **100/100 → proceed to TRD**.

**Resultado do Gate:** ✅ PASS (sujeito à aprovação humana)
