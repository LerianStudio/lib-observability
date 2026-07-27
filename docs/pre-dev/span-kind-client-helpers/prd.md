# Gate 1 — PRD: span-kind-client-helpers

> Gate anterior: [research.md](./research.md)
> Data: 2026-07-26 · Track: Full (9 gates) · Modo: modification
> ⚠️ Documento BUSINESS-ONLY (WHAT/WHY). Nomes de tecnologia/assinaturas/arquitetura → TRD/API-design/Dependency-map.

---

## Executive Summary

As aplicações Lerian não conseguem enxergar o desempenho das suas dependências externas (provedores de identidade, sistemas do Banco Central, serviços internos entre si) porque as chamadas de saída são classificadas de forma errada na telemetria, misturando-se com trabalho interno sem valor de diagnóstico. Esta feature entrega, na biblioteca de observabilidade compartilhada, os meios para que toda chamada de saída seja classificada corretamente como "chamada a dependência externa" — automaticamente na maioria dos casos, e com um recurso manual mínimo para os poucos casos sem cobertura automática. O resultado é visibilidade de latência e erro por dependência (hoje inexistente) e a viabilização de uma redução de custo de telemetria que hoje está bloqueada.

## 1. Problema

Hoje a telemetria das aplicações Lerian classifica **a maioria das chamadas de saída como "operação interna"** em vez de "chamada a uma dependência externa". Medição em produção (2026-07-25): **1.052 séries classificadas como internas contra apenas 111 como chamadas externas** — quando o esperado é o inverso (todo serviço chama banco de dados, cache, provedores de identidade e outros serviços muito mais do que faz trabalho puramente interno).

**Evidência do impacto:**
- Chamadas a dependências externas críticas caem na categoria errada: provedor de identidade (login/refresh de token), sistemas do Banco Central (mensageria de pagamentos), e o serviço de tenants — todos aparecem como "trabalho interno", sem visibilidade própria de latência/erro.
- **Causa observada:** a biblioteca compartilhada oferece cobertura automática correta para alguns tipos de dependência (banco SQL, cache), mas **não para todos** (chamadas HTTP a serviços externos, banco de documentos). Onde não há cobertura automática, cada aplicação instrumenta à mão — e, por falta de um padrão claro e de um atalho correto, invariavelmente classifica errado.
- **Consequência de custo:** existe um plano de reduzir o volume (e o custo) de telemetria descartando o que é genuinamente "operação interna". Esse descarte está **bloqueado** hoje, porque a categoria "interna" está contaminada com chamadas externas valiosas — descartá-la cegamente perderia diagnóstico crítico de dependências do Banco Central e de identidade.

**Workaround atual:** cada equipe lembra (ou esquece) de marcar manualmente cada chamada de saída com a classificação correta. Na prática não é feito de forma consistente — a medição de 1.052×111 é a prova. Não há um caminho fácil e correto, nem um padrão documentado que oriente humanos e assistentes de IA sobre o jeito certo.

## 2. Usuários e valor

| Persona | Objetivo | Frustração hoje | Valor entregue |
|---|---|---|---|
| **Engenheiro de produto (backend)** | Instrumentar chamadas de saída sem virar especialista em telemetria | Precisa lembrar de marcar cada chamada com a classificação certa; não há atalho nem padrão óbvio; erra por omissão | Na maioria dos casos a classificação correta sai automática ao usar a biblioteca; nos casos restantes há um atalho único e documentado, difícil de usar errado |
| **SRE / plantão** | Diagnosticar rapidamente lentidão/erro causados por uma dependência externa | Latência/erro de identidade, Banco Central e serviços internos estão invisíveis (afogados em "operação interna") | Passa a ter latência e taxa de erro **por dependência externa** — diagnóstico de primeira linha |
| **Time de observabilidade / custo** | Reduzir volume e custo de telemetria sem perder diagnóstico | O descarte do "interno" está bloqueado porque a categoria está contaminada | Uma vez adotado, a categoria "interna" fica limpa e o descarte vira seguro — destrava a redução de custo |
| **Qualquer leitor futuro (humano ou IA)** | Entender e aplicar o padrão correto de instrumentação | Não há um princípio documentado; cada um improvisa | Um princípio de precedência claro e redundante na documentação e no próprio código, que orienta desde a primeira leitura |

## 3. User Stories

- **US1 — Classificação automática de chamadas de saída** — Como engenheiro de backend, quero que, ao usar a biblioteca compartilhada para minhas chamadas de saída, elas sejam **automaticamente** classificadas como chamadas a dependência externa, sem eu precisar marcar nada, para eu não errar por omissão.

- **US2 — Visibilidade de latência/erro por dependência externa** — Como SRE, quero enxergar **latência e taxa de erro de cada dependência externa que meu serviço chama** (identidade, Banco Central, serviços internos), para diagnosticar incidentes de dependência em primeira linha, sem depender de instrumentar do lado do provedor.

- **US3 — Atalho manual correto para casos sem cobertura automática** — Como engenheiro de backend, quando **não existir** cobertura automática para um tipo de chamada de saída (ex.: banco de documentos, chamada a serviço custom), quero um **atalho único e simples** que classifique a chamada corretamente, para eu não ter que conhecer os detalhes de baixo nível e não classificar errado.

- **US4 — Princípio de precedência explícito (humano E IA)** — Como qualquer pessoa (ou assistente de IA) que leia o código ou a documentação da biblioteca, quero encontrar, de forma **clara e repetida**, a regra: *"use sempre a cobertura automática quando ela existir; recorra ao atalho manual apenas quando não houver cobertura automática"*, para que o jeito certo seja óbvio desde a primeira leitura e o padrão não se degrade com o tempo.

- **US5 — Não quebrar quem tem telemetria desligada** — Como engenheiro operando um serviço com telemetria desabilitada, quero que adotar esses recursos **nunca** quebre nem degrade minha aplicação (sem falhas, sem travar requisições), para adotar sem risco.

- **US6 — Destravar a redução de custo de telemetria** — Como time de observabilidade, quero que a adoção destes recursos torne a categoria "operação interna" genuinamente limpa, para que o descarte planejado do interno se torne seguro e a redução de custo seja destravada.

- **US7 — Continuar podendo instrumentar de forma custom** — Como engenheiro com uma necessidade específica, quero poder cair para instrumentação manual quando precisar, sem a biblioteca me impedir, para não perder flexibilidade (o atalho é um default conveniente, não uma prisão).

## 4. Critérios de aceite (testáveis)

**CA1 (US1/US3):** Uma chamada de saída instrumentada pelos meios da biblioteca é classificada como "chamada a dependência externa" — verificável na telemetria emitida (a classificação correta aparece; a classificação "interna" NÃO aparece para essa chamada).

**CA2 (US2):** Para chamadas de saída via HTTP, a biblioteca passa a emitir uma medição de **duração por dependência** identificando o destino, o método e o resultado (sucesso/erro) — verificável e conforme o contrato de métricas já publicado da biblioteca. Antes desta feature, essa medição não é emitida por ninguém.

**CA3 (US3):** Existe **um** atalho manual, de uso trivial, que classifica corretamente uma chamada de saída sem exigir que o chamador conheça detalhes de baixo nível; o comportamento default é o correto, e o chamador ainda pode sobrepor deliberadamente quando quiser.

**CA4 (US4) — Princípio de precedência documentado e redundante:** A regra de precedência ("sempre a cobertura automática; manual só sem cobertura") está presente, de forma explícita, em **todos** estes lugares:
  - (a) no README / documentação de uso da biblioteca;
  - (b) na documentação de cada componente de instrumentação (o "cartão de identidade" de cada pacote);
  - (c) no texto de ajuda do próprio atalho manual (quem for usá-lo é avisado, ali mesmo, para preferir a cobertura automática quando existir e listar exemplos de quando o manual é legítimo).
  Verificável por revisão: os três lugares contêm a regra e um exemplo de quando cada caminho se aplica.

**CA5 (US4) — Orientação anti-erro:** A documentação alerta explicitamente contra o erro de **instrumentar em dobro** (usar a cobertura automática E o atalho manual na mesma chamada), deixando claro que o atalho manual é para chamadas **sem** cobertura automática.

**CA6 (US5):** Com telemetria desabilitada (ou sem configuração de telemetria), os novos recursos **não** causam falha, não travam a requisição e não alteram o comportamento funcional da aplicação — degradam para "sem efeito". Verificável por teste automatizado.

**CA7 (guardrail de privacidade/custo):** Os novos recursos **nunca** expõem dados sensíveis nem identificadores de alta cardinalidade (ex.: caminhos de URL com identificadores, dados pessoais) como rótulos de telemetria. Verificável por teste automatizado que falha se qualquer dado proibido aparecer.

**CA8 (compatibilidade):** A feature é **puramente aditiva** — não altera nem remove comportamento existente da biblioteca; aplicações que não adotarem os novos recursos continuam funcionando exatamente como antes.

## 5. Métricas de sucesso

| Métrica | Baseline (2026-07-25) | Alvo | Como medir |
|---|---|---|---|
| Proporção "chamada externa" vs "operação interna" na telemetria de serviços adotantes | 111 externas : 1.052 internas (~9,5%) | Inverter para maioria externa nos serviços adotantes | Contagem de séries por classificação, por serviço, após adoção |
| Visibilidade de latência/erro por dependência externa | Inexistente (0 dependências com medição própria) | Dependências externas-chave (identidade, Banco Central, tenants) com medição própria nos serviços adotantes | Presença da medição de duração por dependência |
| Desbloqueio da redução de custo | Bloqueado (categoria interna contaminada) | Descarte do "interno" viável e seguro após adoção | Decisão registrada do time de observabilidade de que o descarte é seguro |
| Consistência do padrão | Sem padrão documentado | Princípio de precedência presente nos 3 lugares (CA4) | Revisão de documentação |
| Adoção sem regressão | — | Zero regressões atribuídas à feature | Ausência de incidentes/reversões pós-adoção |

## 6. Escopo

### Em escopo
- Meio de classificar corretamente **chamadas de saída via HTTP** de forma automática, incluindo a medição de duração por dependência (US1/US2/CA2).
- **Um atalho manual** de baixo esforço para classificar corretamente chamadas de saída **sem cobertura automática** (US3/CA3) — cobre, entre outros, o caso do **banco de documentos** e chamadas a serviços custom.
- **Documentação do princípio de precedência** nos três lugares exigidos (CA4) e alerta anti-instrumentação-em-dobro (CA5).
- Garantias transversais: degradação segura com telemetria off (CA6), guardrail de privacidade/cardinalidade (CA7), compatibilidade aditiva (CA8).

### Fora de escopo (com justificativa)
- **Migração das aplicações** (adotar os recursos no midaz/plugins): esta feature entrega os **meios** na biblioteca; a adoção em cada aplicação é trabalho downstream separado.
- **Banco de documentos com cobertura automática:** decidido (2026-07-25) que a cobertura automática para o banco de documentos **não é viável no momento** por imaturidade da instrumentação disponível; o caminho para esse caso é o **atalho manual** (US3). Diverge intencionalmente do plano de padronização de métricas anterior — registrar como decisão de arquitetura no TRD.
- **Classificação automática de chamadas de saída via RPC** (criar a marcação correta no caminho RPC-cliente): é um gap real e relacionado, mas fica como **follow-up** — não entra nesta feature.
- **Alterar o plano de padronização de métricas existente:** esta feature **complementa** aquele plano (entrega o produtor de uma medição que ele previa mas não entregava); não o edita nem o substitui.
- **Regras de descarte no coletor de telemetria:** consumir o resultado desta feature para descartar o "interno" é trabalho do time de observabilidade, downstream.

## 7. Premissas
- A biblioteca de observabilidade já publica um **contrato de métricas** com o qual a nova medição de duração por dependência deve se conformar (a medição-alvo já está prevista nesse contrato, sem produtor).
- Já existe cobertura automática correta para banco SQL e cache; esta feature adiciona o caso HTTP e o atalho manual, seguindo os **mesmos padrões** já estabelecidos na biblioteca.
- A feature é aditiva; nenhuma aplicação é obrigada a adotar imediatamente.

## 8. Dependências de negócio
- Consumo por parte do time de observabilidade (para destravar a redução de custo) depende da adoção pelas aplicações — sequência: entregar meios → aplicações adotam → categoria interna limpa → descarte seguro.

## 9. Notas técnicas (transferir para TRD — NÃO são parte do PRD)
> Registradas aqui só para não se perderem; o detalhamento é do TRD/API-design/Dependency-map.
- Escopo confirmado no research: expor **apenas** o atalho de classificação "chamada externa" (os casos "servidor" e "interno" já são cobertos/são o default — sem valor em expor).
- Reuso de instrumentação padrão de mercado para o caso HTTP; alinhamento de versão e privacidade/cardinalidade → Dependency-map/TRD.
- Decisão de precedência de comportamento (default correto, chamador pode sobrepor) e o local de cada componente → TRD/API-design.
- ADRs a registrar no TRD: banco-de-documentos-manual (divergência do plano anterior), default-com-override, automático-vs-manual (anti-dobro), rótulos.

---

## Gate 1 — Validação

- [x] Problema articulado (§1) e impacto quantificado (1.052 vs 111; custo bloqueado)
- [x] Usuários específicos identificados (§2, 4 personas)
- [x] Workaround atual documentado (§1)
- [x] Features endereçam o problema; valor por story claro (§3)
- [x] Métricas mensuráveis (§5, com baseline e alvo)
- [x] Escopo in/out explícito com justificativa (§6)
- [x] Premissas e dependências documentadas (§7, §8)
- [x] Sem tecnologia/arquitetura no corpo do PRD (notas técnicas isoladas em §9 para transferência)
- [x] Requisito de precedência (humano+IA) capturado como US4 + CA4/CA5 (ênfase do usuário)

**Confidence:** Problem clarity 25 (quantificado em prod) · Solution fit 25 (padrão provado, clona sqlobs/redisobs) · Business value 25 (ROI claro: visibilidade + destrava custo) · Market validation 15 (medição interna, não feedback de usuário externo) = **90/100 → autônomo**.

**Resultado do Gate:** ✅ PASS (sujeito à aprovação humana)
