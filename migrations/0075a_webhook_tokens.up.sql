-- 0075a_webhook_tokens.up.sql — SIN-62234 / ADR 0075 rev 3 §3.
-- Token-scoped webhook URL: POST /webhooks/{channel}/{webhook_token}.
-- Plaintext token NEVER persisted: stored as sha256(token) in token_hash.
-- Channel is server-side enum, [a-z0-9_]+ — see internal/webhook/domain.go.
--
-- revoked_at semantics (rev 3 / F-13): scheduled effective revocation
-- timestamp, NOT "is revoked from now". A token is valid while either
-- revoked_at IS NULL OR now() < revoked_at. Rotation sets
-- revoked_at = now() + (overlap_minutes * INTERVAL '1 minute'); overlap=0
-- means immediate cut. The lookup query in
-- internal/adapter/store/postgres/webhook_token_store.go uses that
-- combined predicate.
--
-- The partial unique index over revoked_at IS NULL keeps emit-time
-- collision detection O(1); tokens already in the grace window do NOT
-- block emission of a new replacement (they are no longer "permanently
-- active"). tenants(id) is referenced (assumed pre-existing).

CREATE TABLE IF NOT EXISTS webhook_tokens (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL,
    channel         text        NOT NULL,
    token_hash      bytea       NOT NULL,
    overlap_minutes int         NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    last_used_at    timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS webhook_tokens_active_idx
    ON webhook_tokens (channel, token_hash)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS webhook_tokens_tenant_idx
    ON webhook_tokens (tenant_id);
