-- 0075a2_tenant_channel_associations.up.sql — SIN-62234 / ADR 0075 rev 3 §3.
-- F-12 cross-tenant body cross-check.
--
-- One association (e.g. Meta phone_number_id) maps to exactly one tenant
-- per channel, enforced by PRIMARY KEY (channel, association). The
-- handler validates that BodyTenantAssociation(body) returned by the
-- adapter belongs to the URL-resolved tenant; mismatch → 200 + drop +
-- outcome `webhook.tenant_body_mismatch`.
--
-- Populated via the master/admin UI when an operator registers their
-- phone_number_id (Meta) or equivalent.

CREATE TABLE IF NOT EXISTS tenant_channel_associations (
    tenant_id   uuid        NOT NULL,
    channel     text        NOT NULL,
    association text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel, association)
);

CREATE INDEX IF NOT EXISTS tenant_channel_associations_tenant_idx
    ON tenant_channel_associations (tenant_id, channel);
