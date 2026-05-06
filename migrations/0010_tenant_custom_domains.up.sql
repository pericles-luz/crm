-- 0010_tenant_custom_domains.up.sql
-- SIN-62243 F45 — Custom-domain tables consumed by the on-demand TLS ask
-- handler and the per-tenant enrollment quota.
--
-- Schema mirrors the design captured in SIN-62242 for the tenant_custom_domains
-- and dns_resolution_log tables. Run as app_admin. Idempotent.
--
-- Contracts the rest of the F45 stack relies on:
--   - tenant_custom_domains: one row per host claim. The ask handler returns 200
--     iff verified_at IS NOT NULL AND tls_paused_at IS NULL AND deleted_at IS
--     NULL for the requested host. Per-tenant active-domain count is also read
--     from this table (deleted_at IS NULL).
--   - dns_resolution_log: append-only IP-pinning log so a reviewer can answer
--     "did the resolved IP change between renewals?" in one query. The ask
--     handler does not write here; the F43/F44 validation use-case does.
--
-- Anti-rebinding (ADR 0079 §2): tls_paused_at lets ops freeze a single host's
-- TLS issuance without deleting the row. NULL = active; non-NULL = paused.
--
-- Soft-delete (deleted_at): a tenant tearing down a custom domain may re-claim
-- it later. The unique index on lower(host) is partial on deleted_at IS NULL so
-- soft-deleted rows do not block the re-claim.

BEGIN;

CREATE TABLE IF NOT EXISTS dns_resolution_log (
    id                     UUID PRIMARY KEY,
    host                   TEXT NOT NULL,
    pinned_ip              INET NOT NULL,
    verified_with_dnssec   BOOLEAN NOT NULL,
    audit_event_id         UUID,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_dns_resolution_log_host_created_at
    ON dns_resolution_log(host, created_at DESC);

CREATE TABLE IF NOT EXISTS tenant_custom_domains (
    id                       UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    host                     TEXT NOT NULL,
    verification_token       TEXT NOT NULL,
    verified_at              TIMESTAMPTZ,
    verified_with_dnssec     BOOLEAN NOT NULL DEFAULT FALSE,
    tls_paused_at            TIMESTAMPTZ,
    deleted_at               TIMESTAMPTZ,
    dns_resolution_log_id    UUID REFERENCES dns_resolution_log(id) ON DELETE SET NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenant_custom_domains_active_host
    ON tenant_custom_domains(LOWER(host))
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_tenant_custom_domains_tenant_active
    ON tenant_custom_domains(tenant_id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_tenant_custom_domains_created_at
    ON tenant_custom_domains(created_at DESC)
    WHERE deleted_at IS NULL;

COMMIT;
