-- 0138_instagram_oauth_tokens.up.sql
--
-- Per-tenant Instagram Business Login OAuth access tokens. Replaces the
-- single global META_INSTAGRAM_GRAPH_TOKEN env var (still supported as a
-- fallback) with one token per tenant, obtained via Instagram's own OAuth
-- (authorize -> code exchange -> 60-day long-lived upgrade — see
-- internal/adapter/channel/instagram/oauth.go).
--
-- Keyed by tenant_id alone (not (tenant_id, channel_id)): the existing
-- outbound lookup (instagramOutboundIGBusinessID in
-- cmd/server/instagram_outbound_wire.go) already assumes exactly one
-- Instagram channel per tenant (SELECT ... LIMIT 1 against
-- tenant_channel_associations); this table matches that assumption.
--
-- Stored in plaintext (no encryption at rest) — explicit, discussed
-- decision: same exposure level as the META_GRAPH_TOKEN-family env vars
-- this replaces, now living in Postgres instead of the environment.

CREATE TABLE IF NOT EXISTS instagram_oauth_tokens (
    tenant_id    uuid        PRIMARY KEY,
    access_token text        NOT NULL,
    token_type   text        NOT NULL DEFAULT 'bearer',
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE, DELETE ON instagram_oauth_tokens TO app_runtime;
