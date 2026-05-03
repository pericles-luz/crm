# ADR 0075 — Webhook Security: Signature, Replay Defense, Anti-Enumeration (rev 3)

> **Status:** Proposed (CTO) — rev 3 incorpora F-12/F-13/F-14 + middleware ordering note do [security review do SecurityEngineer sobre rev 2](/SIN/issues/SIN-62224#comment-c082ba9c-1d70-4e8e-b5f8-2b96ef63446e). Rev 2 incorporou os 11 findings do [review original sobre rev 1](/SIN/issues/SIN-62224#comment-aaa3abe1-0176-4d5e-80d1-93936a713bee). Pending re-review final e CEO approval gate antes da implementação.
> **Source findings:** F20, F21, F23 do [security-review em SIN-62220](/SIN/issues/SIN-62220#document-security-review).
> **Triage issue:** [SIN-62224](/SIN/issues/SIN-62224).
> **Plan reference:** [SIN-62190 §7.1](/SIN/issues/SIN-62190#document-plan).
> **Bundle severity:** HIGH. Bloqueante para Fase 1 (primeiro webhook em prod).
> **Lenses applied:** Secure-by-default, Defense in depth, Hexagonal/Ports & Adapters, Reversibility/blast radius, Boring technology, Fail-securely, OWASP Top 10 (A02/A04/A05/A07/A09).

## 0. Changelog

### rev 2 → rev 3

| # | Mudança | Origem |
|---|---|---|
| C13 | §2 D4 — invariante novo: `AppLevel` adapters DEVEM cross-checkar tenant association declarada no body (e.g., `phone_number_id` em Meta Cloud) contra associações conhecidas do `tenant_id` resolvido pelo URL token. Mismatch → 200 + drop + outcome `webhook.tenant_body_mismatch`. Novo método de adapter `BodyTenantAssociation(body) (string, bool)`. | F-12 (MEDIUM, NEW) |
| C14 | §3 — nova tabela `tenant_channel_associations(tenant_id, channel, association)` populada via UI master/admin. | F-12 |
| C15 | §4 — adiciona **T-G9** (cross-tenant body misrouting test). | F-12 |
| C16 | §2 D1 + §3 — semântica de `revoked_at` redefinida como "scheduled effective revocation". Rotação seta `revoked_at = now() + (overlap_minutes * INTERVAL '1 minute')`. Lookup vira `(revoked_at IS NULL OR now() < revoked_at)`. UNIQUE index ajustado. | F-13 (LOW, NEW) |
| C17 | §5 + §6 — JetStream dedup window configurado para **≥ 1 hora** como prereq de prod-on (alinha com tolerance 1h do reconciler em D7). | F-14 (LOW, NEW) |
| C18 | §2 D2 — clarificação: webhook handler é o **primeiro** consumidor de `r.Body` na cadeia; nenhum middleware anterior pode ler body. Lint estendido. | review refinement |

### rev 1 → rev 2

| # | Mudança | Origem |
|---|---|---|
| C1 | §3 — schema separa `webhook_idempotency` (dedup global) de `raw_event` (storage histórico particionado). UNIQUE de dedup deixa de depender de `received_at`. | F-1 (HIGH) |
| C2 | §2 D3 — fallback `Date` HTTP **removido**; `entry[].time` ausente → reject. | F-2 |
| C3 | §3 — `webhook_tokens.token` substituído por `token_hash bytea` = sha256 do token. | F-3 |
| C4 | §2 D7 (novo) + §3 — `raw_event.published_at` + reconciler mínimo. Outbox completo fica para ADR separado. | F-4 |
| C5 | §5 — alert rules + dashboard agora são prereq de Fase 1 prod, especificados aqui. | F-5 |
| C6 | §2 D2 — `channel` ∈ enum server-side fechado; lint rejeita `:` em `Name()`. | F-6 |
| C7 | §2 D2 — Caddy/body bit-exactness invariant (`reverse_proxy` sem transformação, `io.ReadAll` único, body re-read proibido). | F-7 |
| C8 | §2 D3 — `Unix epoch` em **segundos** (10 dígitos); valor > 10^12 rejeitado. | F-8 |
| C9 | §2 D4 — invariante `tenant_id` pré-HMAC = claim, não autenticação; wrapper `authenticatedTenantID(ctx)`. | F-9 |
| C10 | §2 D1 — geração via `crypto/rand.Read`; lint proíbe `math/rand` em `internal/webhook/`. | F-10 |
| C11 | §8 — F-11 (timing oracle unknown vs invalid HMAC) listado como risco residual aceito. | F-11 |
| C12 | §4 — adiciona T-G1..T-G8 conforme §"Gaps de regression test" do review. | review |

---

## 1. Contexto

Plan rev 2 (§7.1) define o fluxo de recebimento de webhook como `POST /webhooks/{channel}/{tenant_slug}` → valida HMAC → resolve `tenant_id` por slug → `INSERT raw_event (idempotency key)` → 200 OK → publica em NATS. O desenho menciona "idempotency key" mas **não compõe** a chave, **não documenta tolerância temporal**, e **expõe o slug do tenant na URL**.

O SecurityEngineer identificou três falhas correlacionadas:

- **F20** — Ordem de verificação ambígua. Para Meta Cloud (app secret único) HMAC pode ser validado antes de resolver tenant. Para PSP PIX (D2 — canal futuro) o secret tende a ser per-tenant, exigindo resolver tenant antes do HMAC. O plan não declara o invariante por adapter — cria risco de regressão silenciosa quando o segundo adapter for adicionado.
- **F21** — Replay defense ausente. HMAC prova autenticidade do payload, não unicidade temporal. Um atacante com 1 webhook capturado pode replayar n vezes — duplicar mensagens, drenar wallet via campanha automática.
- **F23** — Tenant enumeration. URL slug-based retorna 404 para slug inexistente vs 200 para slug válido = oracle de enumeração.

## 2. Decisões

### D1 — URL opaca por tenant (token), não slug

**Decisão:** webhook URL passa a ser `POST /webhooks/{channel}/{webhook_token}`, onde `webhook_token = base64url(b)` com `b := make([]byte, 32); crypto/rand.Read(b)` (32 bytes = 256 bits de entropia CSPRNG).

- Geração via `crypto/rand.Read` **exclusivamente**. Lint custom proíbe `math/rand` em `internal/webhook/...` e em qualquer chamada que persista valor em `webhook_tokens`.
- Token armazenado **apenas como hash** (ver §3 e D-anexa F-3) — coluna `token_hash bytea NOT NULL` = `sha256(token)`. Token plaintext existe **só no momento de geração** (mostrado uma vez ao operador na UI master/admin, nunca persistido). Padrão "rotacionar para ver de novo".
- Lookup: `WHERE channel = $1 AND token_hash = $sha256_of_presented_token AND (revoked_at IS NULL OR now() < revoked_at)`. Hash computado no app antes da query. Custo: +1 sha256 por request (sub-µs).
- **Semântica de `revoked_at` (revisada em rev 3):** `revoked_at` é a **timestamp efetiva de revogação** (scheduled), não "está revogado a partir de agora". Token é válido enquanto `revoked_at IS NULL` ou `now() < revoked_at`. Rotação executa: emitir novo token (`token_hash` novo, `revoked_at = NULL`), e atualizar antigo com `revoked_at = now() + (overlap_minutes * INTERVAL '1 minute')`. `overlap_minutes = 0` = corte imediato (efetivamente `revoked_at = now()`, lookup falha na próxima request); `overlap_minutes > 0` = ambos aceitos durante a janela.
- **Uniqueness** em `(channel, token_hash)` com índice único parcial sobre tokens **ativos** (`WHERE revoked_at IS NULL` — emit-time index, dois tokens distintos com mesmo hash é colisão astronômica). Tokens em janela de grace ainda passam o lookup pela cláusula `now() < revoked_at` mas não bloqueiam emissão de novos.
- Token rotacionável via UI master/admin a qualquer momento. Operador escolhe `overlap_minutes` na rotação.
- Slugs **não vão mais** em URL pública de webhook. Slug continua válido em URLs de UI (`{tenant_slug}.crm.exemplo.com`), onde a enumeração é mitigada por outros controles (rate limit, auth obrigatória).

**Por quê random + tabela vs derivado de master_secret:** derivado força rotação global a cada compromisso individual de tenant; random + tabela permite revogação granular sem cascata.

**Por quê não slug + 200 silencioso (alternativa):** retornar sempre 200 mascara F23 mas mata o sinal operacional ("meu webhook está apontando errado"). Token opaco preserva o sinal porque a falha esperada do operador é "configurei o token errado", não "configurei o slug errado".

### D2 — Idempotency key composta, replay-resistente

**Decisão:** `idempotency_key = sha256(tenant_id || ':' || channel || ':' || raw_payload_canonical)` armazenada como `bytea` (32 bytes), em **tabela dedicada** `webhook_idempotency` (não em `raw_event`).

#### Composição da chave

- `tenant_id` é UUID (formato fixo, 16 bytes / 36 chars hex). Sem ambiguidade de delimitação.
- `channel` ∈ **enum server-side fechado** (`whatsapp`, `instagram`, `facebook`, `webchat`, futuros). Caracteres permitidos: `[a-z0-9_]`. **Sem `:`, sem `|`, sem caracteres de delimitação.** Validação em startup: `RegisterAdapter(a)` rejeita `a.Name()` que contenha `:` ou caractere fora da regex. Lint custom em `internal/adapter/channel/*` confirma.
- `raw_payload_canonical` = bytes exatos do body recebido (o mesmo que entrou no HMAC).

> **Nota técnica:** length-prefix não é necessário enquanto `tenant_id` for UUID-formato e `channel` for enum-fechado-sem-`:`, porque o último `:` separa unambiguamente `channel` de `payload`. Se a validação de `channel` for relaxada no futuro, esta seção precisa virar length-prefix (`len(tenant_id) || tenant_id || len(channel) || channel || payload`) — reabrir ADR.

#### Body bit-exactness (invariante de cadeia HTTP)

- **Caddy:** `reverse_proxy` configurado **sem** `request_body` directives, **sem** decompressão automática (`encode` directive não aplicado a `/webhooks/*`). Verificado por test E2E (T-G1).
- **Go handler:** lê body **uma única vez** no início do handler:
  ```go
  body, err := io.ReadAll(r.Body)
  ```
  O mesmo `[]byte` alimenta `hmac.Verify` E `sha256` da idempotency key.
- **`r.Body` re-read após `io.ReadAll` inicial é proibido.** Lint custom (`paperclip-lint nobodyreread`) flagga qualquer `r.Body.Read`/`io.ReadAll(r.Body)` posterior dentro do call graph do handler de webhook.
- **Webhook handler é o primeiro consumidor de `r.Body` na cadeia.** (Refinamento rev 3.) Nenhum middleware anterior pode ler body — middleware genérico (request logging, body-size-limit que peeka, auth que lê body, request-id) **inserido antes** do webhook handler quebra HMAC E idempotency_key (body vazio no handler). Lint custom estendido para flaggar middleware no router que consume `r.Body` em rotas que casam `/webhooks/*`. Body-size-limit aplicado via `http.MaxBytesReader` setado no router antes do handler é OK (não consome body, só limita).

#### Schema — dedup separado de storage

Ver §3 para DDL completo. Resumo:

- `webhook_idempotency(tenant_id, channel, idempotency_key) PRIMARY KEY` — **não particionada** (B-tree global, lookup O(1)). `inserted_at` para GC.
- `raw_event(...)` — particionada por `received_at` por dia, retenção 30d via DROP PARTITION.
- Fluxo:
  ```sql
  -- 1) dedup
  INSERT INTO webhook_idempotency (tenant_id, channel, idempotency_key, inserted_at)
    VALUES ($1, $2, $3, now()) ON CONFLICT DO NOTHING RETURNING idempotency_key;
  -- 2) se vazio → replay → 200, sem INSERT em raw_event, sem publish.
  -- 3) se inseriu → INSERT raw_event(... raw_payload, headers, received_at) RETURNING id.
  -- 4) marca raw_event.published_at quando NATS confirma publish (ver D7).
  ```
- **Invariante separado:** dedup independe da retenção de `raw_event`. Replay 31 dias depois ainda é dedup'd até GC explícito de `webhook_idempotency` (default 30d em cron noturno; configurável).

**Residual pós-fix:** dedup vira O(1) em B-tree global. DOS por flooding de chaves novas é pego antes pelo HMAC verify (que dropa antes do INSERT).

### D3 — Timestamp tolerance ≤ 5min, sem fallback HTTP

**Decisão:** rejeitar payloads com timestamp fora de janela `[now - 5min, now + 1min]`.

- **Fonte de timestamp por adapter** declarada explicitamente:
  - **Meta Cloud (WhatsApp/IG/FB):** `entry[].time`, formato Unix epoch **em segundos** (10 dígitos). Valor com magnitude > 10^12 (formato ms) é rejeitado com outcome `webhook.timestamp_format_error`.
  - **PSP/futuros:** por contrato do adapter (ver D4) — adapter declara em sua documentação interna qual campo do payload é a fonte de truth.
- **Fallback `Date` HTTP header REMOVIDO.** Razão: `Date` é set pelo cliente HTTP, **fora** do escopo do HMAC do Meta (que cobre só body); atacante replayando webhook capturado pode trocar `Date` para `now()` e bypassar a janela. `entry[].time` ausente em payload Meta → reject (200 + drop + log `webhook.timestamp_missing`). `ExtractTimestamp(headers, body)` do adapter retorna erro.
- Skew assimétrico: 5min para trás (clocks diferem), 1min para frente (chamada do futuro = clock skew suspeito).
- Rejeição = 200 + drop silencioso + log estruturado (não retornar erro permite replay forensics sem feedback ao atacante).

### D4 — Contrato de adapter declara escopo do secret; `tenant_id` pré-HMAC é claim

**Decisão:** cada `ChannelAdapter` Go expõe método `SecretScope() webhook.SecretScope` retornando enum `{AppLevel, TenantLevel}`. **Adapters mistos proibidos** por contrato — fail-fast em startup se um adapter retornar valor inválido.

- `AppLevel` (ex: Meta Cloud): HMAC verifica **antes** de resolver tenant. Tenant é resolvido depois (via `webhook_token` → `tenant_id`).
- `TenantLevel` (ex: PSP PIX hipotético): tenant é resolvido **primeiro** (via `webhook_token` → `tenant_id`), secret é carregado por `tenant_id`, HMAC verifica depois.
- O handler é o mesmo:

```go
type ChannelAdapter interface {
    Name() string                                                 // [a-z0-9_]+, sem ':'
    SecretScope() webhook.SecretScope
    VerifyApp(ctx, body, headers) error                            // SecretScope == AppLevel
    VerifyTenant(ctx, tenantID uuid.UUID, body, headers) error     // SecretScope == TenantLevel
    ExtractTimestamp(headers, body) (time.Time, error)             // sem fallback HTTP Date
    BodyTenantAssociation(body []byte) (assoc string, ok bool)     // F-12 cross-check; ok=false se não aplicável
    ParseEvent(body) (Event, error)
}
```

- Domínio (`internal/webhook/`) **não importa** adapter concreto. Adapters vivem em `internal/adapter/channel/<provider>/`.

#### Invariante "body↔tenant cross-check" para `AppLevel` (F-12, NEW rev 3)

`AppLevel` HMAC autentica **origem** do body, não **destino pretendido**. Atacante com webhook autêntico de tenant A + URL/token de tenant B (leak operacional) consegue rota cross-tenant: HMAC OK, token lookup → B, evento entra na inbox de B com PII de A. Defesa por `webhook_token` é insuficiente porque o token autentica a destinação da request, não a origem semântica do conteúdo.

**Invariante:** adapter `AppLevel` cujo body identifica explicitamente o tenant destinado (via campo do provider, e.g., `entry[].changes[].value.metadata.phone_number_id` em Meta Cloud) **DEVE** declarar isso via `BodyTenantAssociation(body)`. Quando `ok == true`, handler valida que `assoc ∈ tenant_channel_associations(tenant_id, channel)`. Mismatch = 200 + drop + outcome `webhook.tenant_body_mismatch`.

Adapters cujo provider não envia identificação de tenant no body (ou cujo `AppLevel` é genuinamente single-tenant na prática) retornam `ok == false` e o cross-check é skip — mas a ausência **deve** ser justificada na docstring do adapter (revisado em PR).

`tenant_channel_associations` é populada via UI master/admin (e.g., operador adiciona seu `phone_number_id` Meta ao tenant). Schema em §3. Test T-G9 cobre.

#### Invariante "tenant claim" pré-HMAC (F-9)

Em `TenantLevel`, entre `webhook_token → tenant_id` e `VerifyTenant(...) == nil`, **`tenant_id` é claim, não autenticação.** Aplicado por contrato:

- Nenhum log estruturado tenant-scoped.
- Nenhuma métrica tenant-labeled (Prometheus labels NÃO incluem `tenant_id` antes de `VerifyTenant` retornar nil).
- Nenhuma decisão de autorização. Authorizer (RBAC matrix) só é chamado com `tenant_id` autenticado.
- Nenhum efeito colateral em DB de outro tenant.

**Enforce em código:**
- Wrapper `webhook.AuthenticatedTenantID(ctx) (uuid.UUID, bool)` — retorna `(uuid, true)` apenas após `VerifyTenant` ter setado o ctx-value. Outras chamadas retornam `(uuid.Nil, false)`. Pacote sem getter para "claim tenant id" — pré-HMAC é só `local var`, não `context.Value`.
- Test T-G2 cobre: log/metric collector inspeciona o request pré-HMAC e confirma ausência de `tenant_id` em qualquer label/field.

### D5 — Anti-enumeration: 200 + drop silencioso + alerting interno

**Decisão:** com D1 (URL token-based), token desconhecido retorna 200 + drop silencioso + log `webhook.unknown_token`. HMAC inválido idem (200 + drop + log).

- Sinal operacional preservado por **alert rules + dashboard internos** (ver §5). Drop silencioso vira aceitável apenas se alerting estiver em vigor — `webhook.security_v2.enabled` em prod gated em §5 ser merged.
- 404 **proibido** em qualquer caminho de webhook. Caddy/router devolve 200 vazio para qualquer prefixo `/webhooks/*` (incluso path inexistente).

### D6 — Ack rápido continua como invariante

A ordem é:
1. `verify HMAC` (AppLevel) **ou** `lookup tenant via token + load secret + verify HMAC` (TenantLevel)
2. `parse + validate timestamp` (ver D3)
3. `INSERT webhook_idempotency ... ON CONFLICT DO NOTHING RETURNING idempotency_key` (ver D2)
4. Se inseriu: `INSERT raw_event(... published_at NULL) RETURNING id`
5. `200 OK`
6. **Async:** `publish NATS` em goroutine; em sucesso, `UPDATE raw_event SET published_at = now() WHERE id = $1`

Ack ≤ 100ms p95 (alvo de plan rev 2). Path crítico inclui +1 sha256 (token), +1 sha256 (idempotency key), +1 INSERT (idempotency), +1 INSERT (raw_event). Tudo dentro do budget.

### D7 — Outbox-lite via `published_at` + reconciler mínimo

**Decisão:** ADR de outbox completo fica fora do escopo deste ADR, mas o **gap de "INSERT raw_event OK + crash antes de NATS publish"** não pode esperar.

- `raw_event.published_at timestamptz NULL` (default NULL).
- Async path em D6 step 6 atualiza `published_at = now()` em sucesso de NATS publish.
- **Reconciler:** worker dedicado em `internal/worker/webhook_reconciler.go` que a cada 30s roda:
  ```sql
  SELECT id, tenant_id, channel, raw_payload, headers
    FROM raw_event
   WHERE published_at IS NULL
     AND received_at < now() - INTERVAL '1 minute'
   ORDER BY received_at
   LIMIT 100
   FOR UPDATE SKIP LOCKED;
  ```
  Para cada row: tenta publish NATS; em sucesso, `UPDATE published_at = now()`; em falha, deixa para próxima rodada (max 1h de tolerância → alerta).
- **Garantia:** at-least-once para o consumer NATS. Duplicate publish (reconciler + race com publish original) é dedup'd no consumer NATS por `idempotency_key` (header NATS `Nats-Msg-Id` populado com `hex(idempotency_key)`).
- **JetStream dedup window configurado para ≥ 1 hora** (rev 3, F-14). Default JetStream é 2min, insuficiente para o cenário "publish OK → crash antes de UPDATE → reconciler down/lento por mais que 2min → republish vaza para consumer." Janela de 1h alinha com a tolerância de 1h do reconciler em D7 antes de virar alert. Configuração explícita no stream config: `Duplicates: 1*time.Hour`. Listada como prereq de prod-on em §6.
- Defesa adicional (defense in depth, opcional para rev 3 mas recomendada para Fase 1+): consumer worker que escreve `Message` no DB usa UNIQUE em `(tenant_id, idempotency_key)` para dedupe terminal. Cobre dup também por causas fora deste ADR (e.g., re-replay manual de NATS).
- Outbox completo (transactional outbox, multi-event, replay temporal sofisticado) entra em ADR separado quando Fase 1 acoplar Worker complexo.

## 3. Schema

```sql
-- 0075a_webhook_tokens.up.sql
CREATE TABLE webhook_tokens (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    channel         text NOT NULL,
    token_hash      bytea NOT NULL,                            -- sha256(token), 32 bytes
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,                                -- semântica: timestamp efetiva de revogação (scheduled).
                                                                -- NULL = ativo permanentemente. now() < revoked_at = ativo na janela de grace.
                                                                -- now() >= revoked_at = inativo. (rev 3 / F-13)
    last_used_at    timestamptz
);
-- UNIQUE em tokens "permanentemente ativos" (revoked_at IS NULL).
-- Tokens em janela de grace (revoked_at populated, now() < revoked_at) ainda passam o lookup
-- via cláusula OR, mas não bloqueiam emissão de novos.
CREATE UNIQUE INDEX webhook_tokens_active_idx
  ON webhook_tokens (channel, token_hash) WHERE revoked_at IS NULL;
-- nota: token plaintext NUNCA persistido. Mostrado uma vez ao operador na UI master/admin
-- no momento da geração e descartado. Re-emissão = rotacionar.

-- 0075a2_tenant_channel_associations.up.sql (rev 3 / F-12)
CREATE TABLE tenant_channel_associations (
    tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    channel     text NOT NULL,
    association text NOT NULL,                                  -- e.g., phone_number_id em Meta Cloud
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel, association)                          -- 1 association nunca pertence a 2 tenants
);
CREATE INDEX tenant_channel_associations_tenant_idx
  ON tenant_channel_associations (tenant_id, channel);
-- Populado via UI master/admin quando operador cadastra phone_number_id (Meta) ou similar.
-- AppLevel adapters cujo BodyTenantAssociation(body) retorna ok=true validam que o assoc
-- declarado no body bate com (tenant_id resolvido por URL token, channel) antes de tratar
-- o evento como autenticado para o tenant. Mismatch → outcome webhook.tenant_body_mismatch.

-- 0075b_webhook_idempotency.up.sql
CREATE TABLE webhook_idempotency (
    tenant_id       uuid NOT NULL,
    channel         text NOT NULL,
    idempotency_key bytea NOT NULL,                            -- sha256(tenant_id||':'||channel||':'||raw_payload), 32 bytes
    inserted_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, channel, idempotency_key)
);
CREATE INDEX webhook_idempotency_gc_idx ON webhook_idempotency (inserted_at);
-- GC: DELETE FROM webhook_idempotency WHERE inserted_at < now() - INTERVAL '30 days'
-- em cron noturno. B-tree, barato.

-- 0075c_raw_event.up.sql (storage histórico, NÃO usado para dedup)
CREATE TABLE raw_event (
    id              uuid NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL,
    channel         text NOT NULL,
    idempotency_key bytea NOT NULL,                            -- denormalized for forensics; UNIQUE não exigido aqui
    raw_payload     bytea NOT NULL,
    headers         jsonb NOT NULL,
    received_at     timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,                               -- NULL até reconciler/handler confirmar NATS publish
    PRIMARY KEY (received_at, id)
) PARTITION BY RANGE (received_at);
CREATE INDEX raw_event_unpublished_idx
  ON raw_event (received_at) WHERE published_at IS NULL;
-- partições diárias criadas/dropadas via pg_partman (preferido) ou cron interno.
-- retenção 30d via DROP PARTITION.
```

Backward-compat: como Fase 1 ainda não está em prod, **não há migração de dados**. Schema entra direto. Rollback = `DROP TABLE` em ordem reversa.

## 4. Regression tests (entram no PR de implementação)

Cobertura ≥ 85% no pacote `internal/webhook` e nos adapters tocados.

### Caminho feliz dos fixes (rev 1 baseline)

- **T-1 Replay imediato.** Mesmo payload assinado válido enviado 2x → primeiro insere `webhook_idempotency` + `raw_event` + publica NATS, segundo retorna 200 sem segunda inserção e sem segundo publish. Verificado por `SELECT count(*) FROM webhook_idempotency` (=1), `SELECT count(*) FROM raw_event` (=1), e contador de mensagens NATS consumidas (=1).
- **T-2 Timestamp window.** Payload com `entry[].time` 6min no passado → 200, dropado, métrica `webhook.replay_window_violation` incrementada.
- **T-3 Token desconhecido.** POST `/webhooks/whatsapp/inexistente` → 200, dropado, métrica `webhook.unknown_token` incrementada. **Não 404.**
- **T-4 HMAC inválido.** Token válido, body adulterado → 200, dropado, métrica `webhook.signature_invalid`. **Não 401/403.**
- **T-5 Adapter contract.** Registrar adapter fictício com `SecretScope` inválido → startup falha com erro claro contendo `Name()` do adapter.
- **T-6 AppLevel order.** Adapter `AppLevel`: HMAC verifica **antes** de tocar `webhook_tokens`. Cobre token rotacionado mid-flight (operador rotacionou cedo demais) — request com HMAC válido + token recém-revogado deve falhar no token check, não no HMAC.
- **T-7 Token revogado.** Token marcado `revoked_at` → 200, dropado, métrica `webhook.revoked_token`.

### Gaps preenchidos pelo review (rev 2)

- **T-G1 Body bit-exactness E2E.** Caddy local (test fixture) → app, body de 1 KB com whitespace específico. Confirma `len(received) == len(sent)` e `received == sent` byte-a-byte. HMAC e idempotency_key computados pelo app batem com computação independente sobre o body original.
- **T-G2 TenantLevel claim isolation.** Adapter `TenantLevel`: pre-HMAC, log capture e metric capture confirmam **ausência** de `tenant_id` em qualquer estrutura serializada. `webhook.AuthenticatedTenantID(ctx)` retorna `(uuid.Nil, false)`. Após `VerifyTenant` OK, retorna `(tenantID, true)`.
- **T-G3 Date fallback removido.** Payload Meta sem `entry[].time` é dropado mesmo com `Date` HTTP válido em now. Métrica `webhook.timestamp_missing`. **Não** 200-com-publish.
- **T-G4 Cross-tenant idempotency segmentation.** Mesmo `raw_payload` para `(tenant=A, channel=whatsapp)` e `(tenant=B, channel=whatsapp)` → dois INSERTs distintos em `webhook_idempotency`, dois `raw_event`, dois publishes NATS.
- **T-G5 Replay 24h+.** Payload re-enviado 25h após o original → 200 + drop, sem segundo publish em NATS. Garante que `webhook_idempotency` (não particionada) ainda dedupa após `raw_event` ter rotado de partição. **Este teste falha hoje na §3 do rev 1 — serve de regression-test contra reinstalação do bug F-1.**
- **T-G6 Token rotation overlap.** Emitir token novo com `overlap_minutes=5`. Antigo+novo aceitos durante 5min, depois só novo. `webhook_tokens.revoked_at` + janela honrada.
- **T-G7 Timestamp ms rejection.** Payload com `entry[].time` em formato 13 dígitos (ms) → 200 + drop, métrica `webhook.timestamp_format_error`.
- **T-G8 Token-at-rest hash.** `SELECT token_hash FROM webhook_tokens` retorna apenas hashes; nenhuma coluna armazena plaintext. Gerar token + `hash(plaintext)` no test bate com `token_hash` armazenado; `plaintext` fora do test não é recuperável do DB.

### Gap preenchido em rev 3

- **T-G9 Cross-tenant body misrouting (F-12).** Setup: tenant A com `phone_number_id=PA` em `tenant_channel_associations`; tenant B com `phone_number_id=PB`. Capturar payload Meta autêntico (HMAC válido) endereçado a A (i.e., `entry[].changes[].value.metadata.phone_number_id = PA`). POSTar esse body bit-exato para a URL/token de B. Esperado: 200 + drop, outcome `webhook.tenant_body_mismatch`, **zero** entries novos em `webhook_idempotency` para B, **zero** publishes em NATS para B. Verifica que cross-check `BodyTenantAssociation(body) ∈ tenant_channel_associations(B, channel)` falha.

## 5. Observabilidade (gate de prod)

`webhook.security_v2.enabled = on` em prod **só** depois das alert rules + dashboard merged e validados em staging.

### Métricas Prometheus

- `webhook_received_total{channel, outcome}` — outcome ∈ `{accepted, replay, unknown_token, revoked_token, signature_invalid, replay_window_violation, timestamp_missing, timestamp_format_error, parse_error, tenant_body_mismatch}`. Label `tenant_id` **só** quando `outcome=accepted` ou outcome pós-HMAC autenticado (ver D4 invariante). Pre-HMAC outcomes são contados sem `tenant_id`. **Nota:** `tenant_body_mismatch` é pós-HMAC mas pré-autenticação semântica — para evitar leak de associação válida, label `tenant_id` é o tenant resolvido pelo URL token (que é o destino legítimo da request, mesmo que o body seja inválido para ele).
- `webhook_ack_duration_seconds{channel}` histogram (p50/p95/p99 dashboards).
- `webhook_idempotency_conflict_total{channel, tenant_id}` — replay autenticado (post-HMAC), sinal de tentativa de fraude OU cliente buggado.
- `webhook_unpublished_event_count{channel}` gauge — `SELECT count(*) FROM raw_event WHERE published_at IS NULL` por canal, scrape a cada 30s (matching o reconciler).

### Alert rules (Prometheus)

- `webhook.signature_invalid_rate` — `rate(webhook_received_total{outcome="signature_invalid"}[5m]) > 0.05` per channel sustentado por 5min → page on-call (atacante ativo OU regressão de assinatura nossa).
- `webhook.unknown_token_burst` — `increase(webhook_received_total{outcome="unknown_token"}[1h]) > 10` → low-priority ticket (provável misconfig de operador legítimo OU probing).
- `webhook.replay_window_violation_rate` — `rate(webhook_received_total{outcome="replay_window_violation"}[5m]) > 0.1` → page on-call (replay attack suspect).
- `webhook.unpublished_backlog` — `webhook_unpublished_event_count > 100` per channel sustentado por 10min → page (NATS down OR reconciler stuck).
- `webhook.tenant_body_mismatch_rate` (rev 3 / F-12) — `rate(webhook_received_total{outcome="tenant_body_mismatch"}[5m]) > 0.01` por canal/tenant → page on-call. Tenant_body_mismatch acima de baseline ≈ 0 indica cross-tenant misrouting attempt (ataque) OU operador adicionando novo `phone_number_id` Meta sem cadastrá-lo em `tenant_channel_associations` (ops gap).

### Dashboard Grafana

Painel "Webhook security" com:
- Taxa de outcome por canal (stacked area).
- p50/p95/p99 de `webhook_ack_duration_seconds` por canal.
- `webhook_unpublished_event_count` por canal (gauge + sparkline).
- Per-tenant breakdown (apenas para outcomes autenticados).

Visibilidade: master sees all tenants; tenant operator sees only their tenant.

### Logs

Estruturados, JSON. Sempre: `request_id`, `channel`, `outcome`, `received_at`, `tenant_id` (apenas pós-HMAC autenticado).

**Proibido em log (lint custom `paperclip-lint nosecrets`):**
- `webhook_token` (raw nem hash).
- `raw_payload` em texto livre.
- `Authorization` header ou qualquer secret.
- Em pré-HMAC: `tenant_id`, `tenant_slug`, ou qualquer label tenant-scoped.

Payload vai para `raw_event.raw_payload` (criptografia em repouso por Postgres TDE/disk encryption — coberto separadamente em ADR de PII storage).

## 6. Reversibilidade

- **Feature flag:** `webhook.security_v2.enabled` (ConfigMap) controla a entrada no novo path. Off = path antigo (slug-based, se ainda existir) ou 404; on = path novo (token-based). Default off durante deploy, on após smoke. Permite voltar em < 1min se algo quebrar.
- **Hard prereq de prod-on:**
  - §5 alert rules + dashboard merged e validados em staging.
  - JetStream stream config explicitamente com `Duplicates: 1*time.Hour` (rev 3 / F-14) — alinhado com tolerância 1h do reconciler em D7. Default 2min é insuficiente.
  - `tenant_channel_associations` populada para todos os tenants ativos antes do flip on (rev 3 / F-12) — sem isso, todo evento Meta autêntico seria dropado com `tenant_body_mismatch`.
- **Schema rollback:** `DROP TABLE raw_event; DROP TABLE webhook_idempotency; DROP TABLE tenant_channel_associations; DROP TABLE webhook_tokens;` (ordem reversa às migrations). Sem dependência cruzada além de `tenants(id)`. Reversível enquanto Fase 1 não estiver em prod.
- **Token rotation:** já é a operação reversível primária. Suspeita de compromisso = rotação imediata, sem downtime.

## 7. O que este ADR **não** decide

- **Outbox completo** (transactional outbox, replay temporal sofisticado) — entra em ADR separado. D7 cobre o gap mínimo (`published_at` + reconciler) para Fase 1 prod.
- **Criptografia em repouso** de `raw_event.raw_payload` (TDE Postgres vs aplicação) — entra em ADR de PII storage.
- **Schema de `tenants`** (ainda não criado) — premissa: existe `tenants(id uuid PRIMARY KEY)` quando este ADR for implementado.
- **Rate limiting** per-tenant no path do webhook — entra em ADR separado de rate limiting global (ver F-11 residual).
- **UI master/admin para `tenant_channel_associations`** — design de tela e fluxo operacional fica em issue separado de UI; este ADR define apenas o schema e o invariante. Implementação pode usar form simples até UI dedicada existir.

## 8. Consequências

**Positivas:**
- F20, F21, F23 fechados por defesas independentes (defense in depth).
- F-1..F-14 do review independente endereçados.
- Token opaco hash-em-repouso é rotacionável sem code change; reduz blast radius por compromisso de DB.
- Schema replay-defense é boring e auditável (constraint única em tabela dedicada).
- Contrato de adapter força explicitação — invariante vira tipo, não convenção.
- `published_at` + reconciler eliminam perda silenciosa em Fase 1 prod sem inflar escopo.
- Alert rules + dashboard tornam drop silencioso seguro (oracle externo eliminado, sinal interno preservado).
- Cross-check `body↔tenant` (F-12) elimina vetor BOLA cross-tenant mesmo se URL/token vazar operacionalmente.

**Negativas / custos:**
- Operadores precisam configurar webhook URL com token longo (UX overhead). Mitigação: UI master/admin gera URL completa pronta para copiar.
- 4 tabelas (`webhook_tokens`, `webhook_idempotency`, `raw_event`, `tenant_channel_associations`) vs 2 do rev 1 com schema furado. Compensa porque desacopla concerns (token, dedup, storage, ownership).
- Operador precisa cadastrar `phone_number_id` (ou equivalente por canal) em `tenant_channel_associations` antes de receber webhooks. Sem isso, eventos legítimos são dropados como `tenant_body_mismatch`. Mitigação: alerting por mismatch (§5) + UI master/admin para gestão.
- Particionamento de `raw_event` exige pg_partman ou cron manual. +1 dependência operacional.
- Reconciler é +1 worker. Boring (SELECT FOR UPDATE SKIP LOCKED + UPDATE), mas tem que existir e ser monitored.
- JetStream dedup window de 1h aumenta consumo de memória do stream (carrega `MsgID`s vivos). Carga estimada (40 webhooks/s pico × 3600s = ~144k ids × 32B hex = ~4.6 MB). Aceitável.

**Risco residual:**

- **F-11 (LOW, aceito) — Timing oracle pré-HMAC.** Unknown token (DB miss, ~µs) vs valid token + invalid HMAC (DB hit + sha256 + HMAC compute, mais lento). Diferencial mensurável remotamente em redes baixa-latência. Para 2^256 espaço de token, enumeration via timing é **academic** (esforço = atacar AES brute force). Não corrigido neste ADR. Mitigação aceitável via rate limit per source IP em Caddy (ADR de rate limiting).
- **F-14 residual (LOW)** — defesa terminal no consumer worker (UNIQUE em `Message` table) é recomendada mas opcional para rev 3. Sem ela, gap raro (publish OK + crash + reconciler down >1h) ainda pode duplicar uma mensagem para o cliente final. Aceitável até Fase 1+ acoplar `Message` UNIQUE.
- **DB read-only compromise** continua mapeando tenant↔canal pelas linhas (ainda que sem token usável pós-F-3). Mitigado por defesa em camadas externas a este ADR.
- **Token leak via log do operador** (e.g. operator paste em log externo). Mitigação interna: `webhook_token` proibido em log via lint + redactor middleware. Mitigação humana: docs. F-12 cross-check elimina o impacto cross-tenant mesmo no caso pessimista.
- **NATS down > 1h** com `published_at IS NULL` acumulando — alerting cobre (§5), mas operador precisa agir.


